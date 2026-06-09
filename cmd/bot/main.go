package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/rs/zerolog"

	"github.com/bestxp/narrative-ai-agent/internal/adapter/gitops"
	"github.com/bestxp/narrative-ai-agent/internal/adapter/llm"
	llmopenai "github.com/bestxp/narrative-ai-agent/internal/adapter/llm/openai"
	llmanthropic "github.com/bestxp/narrative-ai-agent/internal/adapter/llm/anthropic"
	"github.com/bestxp/narrative-ai-agent/internal/adapter/storage"
	"github.com/bestxp/narrative-ai-agent/internal/config"
	"github.com/bestxp/narrative-ai-agent/internal/dispatcher"
	"github.com/bestxp/narrative-ai-agent/internal/domain"
	"github.com/bestxp/narrative-ai-agent/internal/logging"
	"github.com/bestxp/narrative-ai-agent/internal/messaging"
	"github.com/bestxp/narrative-ai-agent/internal/messaging/telegram"
	promptpkg "github.com/bestxp/narrative-ai-agent/internal/prompts"
	"github.com/bestxp/narrative-ai-agent/internal/slowlog"
	"github.com/bestxp/narrative-ai-agent/internal/structured"
	"github.com/bestxp/narrative-ai-agent/internal/usecase"
	"github.com/bestxp/narrative-ai-agent/internal/usecase/tools"
)

func main() {
	cfgPath := flag.String("config", "config.yaml", "path to config.yaml")
	logLevel := flag.String("log-level", "", "override log level (trace/debug/info/warn/error)")
	prettyLog := flag.Bool("log-pretty", false, "human-friendly console writer")
	disableLLM := flag.Bool("no-llm", false, "run without LLM (echo + validation only)")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		l := zerolog.New(os.Stderr)
		l.Error().Err(err).Msg("config load failed")
		os.Exit(1)
	}

	level := *logLevel
	if level == "" {
		level = "info"
	}
	log := logging.New(logging.Config{Level: level, Pretty: *prettyLog})

	log.Info().Str("config", *cfgPath).Bool("no_llm", *disableLLM).Msg("starting lazy-universe bot")

	absData, err := filepath.Abs(cfg.Paths.DataRoot)
	if err != nil {
		log.Fatal().Err(err).Msg("data root")
	}
	fs, err := storage.NewFileStoreWithLogger(absData, log)
	if err != nil {
		log.Fatal().Err(err).Msg("storage init")
	}

	if !gitops.IsRepo(cfg.Paths.GitWorkdir) {
		log.Warn().Str("workdir", cfg.Paths.GitWorkdir).Msg("not a git repo — commits will fail")
	}
	gitOp := gitops.NewWithLogger(cfg.Paths.GitWorkdir, cfg.Git.Remote, cfg.Git.Branch, cfg.Git.CommitAuthor, cfg.Git.CommitEmail, cfg.Git.RemoteDisabled, log)

	slow, err := buildSlowlog(cfg, log)
	if err != nil {
		log.Fatal().Err(err).Msg("slowlog init")
	}
	if cfg.Slowlog.Enabled {
		log.Info().Str("path", cfg.Slowlog.File).Msg("slowlog enabled")
	}

	// role / systemPrompt / driver / summarizer must be
	// wired BEFORE NewFileToolset: the file backend's
	// memory concern now takes the LLM-driven summarizer
	// so it can call back into a cheap model to compact
	// overgrown NPC profiles. Without that wiring
	// MaintainNPCs is a no-op and the legacy naive strip
	// path stays in place (still safe, just less
	// accurate).
	role, ok := cfg.Role(config.NarrativeRole)
	if !ok {
		log.Fatal().Str("role", config.NarrativeRole).Msg("narrative role not configured")
	}
	log.Info().Strs("prompts", promptpkg.List()).Msg("bundled prompts")
	systemPrompt, err := promptpkg.LoadSystemPrompt(role.SystemPromptPath, "narrative.md")
	if err != nil {
		log.Fatal().Err(err).Str("path", role.SystemPromptPath).Msg("read system prompt")
	}
	// Build the LLM driver. Two implementations are
	// supported (h4 hardcoded wire surface in both):
	//   - "openai" (default): openai-go v3 SDK
	//     (github.com/openai/openai-go/v3) over
	//     /v1/chat/completions. response_format=json_object
	//     + tool_choice=auto + strict_tools=true baked in.
	//   - "anthropic": anthropic-sdk-go v1.48.0 over
	//     /v1/messages. tool_choice=auto + strict_tools=true;
	//     no response_format (the 4-field narrative shape is
	//     described in the system prompt).
	//
	// The choice is per-process, not per-role: drivers
	// are constructed at boot and serve every chat turn.
	var driver llm.Driver
	switch cfg.LLM.Driver {
	case "", "openai":
		driver = llmopenai.New(llm.RoleConfig{
			APIURL:                  role.APIURL,
			APIKey:                  role.APIKey,
			Model:                   role.Model,
			MaxTokens:               role.MaxTokens,
			Temperature:             role.Temperature,
			RequestTimeoutSeconds:   role.RequestTimeoutSeconds,
			DisableThinking:         role.DisableThinking,
			ReasoningEffort:         role.ReasoningEffort,
			MaxEmptyRetries:         role.MaxEmptyRetries,
			EmptyRetryTimeoutSeconds: role.EmptyRetryTimeoutSeconds,
		}, log)
		log.Info().Str("driver", "openai").Str("model", role.Model).Msg("llm driver ready")
	case "anthropic":
		driver = llmanthropic.NewWithSlowlog(llm.RoleConfig{
			APIURL:                  role.APIURL,
			APIKey:                  role.APIKey,
			Model:                   role.Model,
			MaxTokens:               role.MaxTokens,
			Temperature:             role.Temperature,
			RequestTimeoutSeconds:   role.RequestTimeoutSeconds,
			DisableThinking:         role.DisableThinking,
			ReasoningEffort:         role.ReasoningEffort,
			MaxEmptyRetries:         role.MaxEmptyRetries,
			EmptyRetryTimeoutSeconds: role.EmptyRetryTimeoutSeconds,
		}, log, slow)
		log.Info().Str("driver", "anthropic").Str("model", role.Model).Msg("llm driver ready")
	default:
		log.Fatal().Str("driver", cfg.LLM.Driver).Msg("unknown llm.driver; expected openai|anthropic")
	}
	// The usecase layer (gm, summarizer) depends on a
	// narrow LLMClient interface that does not include
	// Close. Wrap the driver in a tiny adapter that
	// implements just the methods we need; this keeps
	// the usecase surface stable while the driver
	// contract grows (openai-go pooled resources live
	// in main.go's responsibility).
	llmCli := driverClient{driver: driver}
	defer driver.Close()

	// summarizer: reuses the same LLM SDK as the primary
	// driver (openai or anthropic per cfg.LLM.Driver), but
	// with a different system_prompt and lower max_tokens.
	// We construct a fresh driver instance for the summary
	// role so the per-call parameters (model, max_tokens,
	// temperature) can differ from the GM's role.
	var summarizer *usecase.Summarizer
	{
		var sumRole llm.RoleConfig
		var sumPrompt string
		var ok bool
		if sumRoleCfg, hasSummary := cfg.Role("summary"); hasSummary {
			sumRole = llm.RoleConfig{
				APIURL:                  sumRoleCfg.APIURL,
				APIKey:                  sumRoleCfg.APIKey,
				Model:                   sumRoleCfg.Model,
				MaxTokens:               sumRoleCfg.MaxTokens,
				Temperature:             sumRoleCfg.Temperature,
				RequestTimeoutSeconds:   sumRoleCfg.RequestTimeoutSeconds,
				DisableThinking:         sumRoleCfg.DisableThinking,
				ReasoningEffort:         sumRoleCfg.ReasoningEffort,
				MaxEmptyRetries:         sumRoleCfg.MaxEmptyRetries,
				EmptyRetryTimeoutSeconds: sumRoleCfg.EmptyRetryTimeoutSeconds,
			}
			sumPrompt, err = promptpkg.LoadSystemPrompt(sumRoleCfg.SystemPromptPath, "summary.md")
			if err != nil {
				log.Warn().Err(err).Str("path", sumRoleCfg.SystemPromptPath).Msg("summary prompt unreadable; dropping to fallback")
			} else {
				ok = true
			}
			log.Info().Str("model", sumRoleCfg.Model).Msg("summarizer: dedicated role")
		}
		if !ok {
			sumRole = llm.RoleConfig{
				APIURL:                role.APIURL,
				APIKey:                role.APIKey,
				Model:                 role.Model,
				MaxTokens:             role.MaxTokens,
				Temperature:           role.Temperature,
				RequestTimeoutSeconds: role.RequestTimeoutSeconds,
				DisableThinking:       role.DisableThinking,
				ReasoningEffort:       role.ReasoningEffort,
			}
			sumPrompt, err = promptpkg.LoadSystemPrompt("", "summary.md")
			if err != nil || sumPrompt == "" {
				log.Warn().Err(err).Msg("fallback summary prompt unreadable; compaction will be drop-only")
			} else {
				ok = true
			}
			log.Info().Str("model", role.Model).Msg("summarizer: fallback to narrative role (clamped)")
		}
		if ok {
			var sumLLM llm.Driver
			switch cfg.LLM.Driver {
			case "anthropic":
				sumLLM = llmanthropic.New(sumRole, log)
			default:
				sumLLM = llmopenai.New(sumRole, log)
			}
			if _, hasSummary := cfg.Role("summary"); hasSummary {
				summarizer = usecase.NewSummarizer(sumLLM, sumRole, sumPrompt, slow, log)
			} else {
				summarizer = usecase.NewFallbackSummarizer(sumLLM, sumRole, sumPrompt, slow, log)
			}
			// Этап 0b/0c: wire the in-place and end-of-day
			// compaction prompts onto the same Summarizer.
			// Both use the same model/role as the regular
			// compaction path; the prompt differences
			// (150-300 words, current-day vs 200-400
			// words, past-day) live in the .md files.
			if inPlacePrompt, err := promptpkg.LoadSystemPrompt("", "compaction_in_place.md"); err == nil && inPlacePrompt != "" {
				summarizer.SetCompactionInPlacePrompt(inPlacePrompt)
			} else {
				log.Warn().Err(err).Msg("compaction_in_place.md unreadable; in-place compaction will no-op")
			}
			if eodPrompt, err := promptpkg.LoadSystemPrompt("", "end_of_day.md"); err == nil && eodPrompt != "" {
				summarizer.SetEndOfDayPrompt(eodPrompt)
			} else {
				log.Warn().Err(err).Msg("end_of_day.md unreadable; end-of-day protocol will no-op")
			}
		}
	}

	ss := usecase.NewSessionStartWithLogger(fs, log)
	fl := usecase.NewFirstLaunchWithLogger(fs, log)
	sysSt := usecase.NewSystemState(fs, log, slow)

	// One Tool bundles every concern. main.go constructs it
	// once, hands it to the dispatcher and the GM, and that's
	// the entire wiring. The previous five-concrete-objects
	// layout made it trivial to forget to wire one of them;
	// the single Tool surface fails closed.
	//
	// summarizer is the LLM-driven compaction hook used for
	// THREE different compaction kinds: NPC profiles, lore.md,
	// and the 30-day memorise.md windows. The same
	// *usecase.Summarizer implements all three (it has a
	// SummarizeNPC / SummarizeLore / SummarizeMemorise
	// method each); we pass the SAME adapter in all three
	// slots so the production deployment gets the LLM path
	// for every compaction. Pass nil to disable a slot
	// (the file backend will log a warning and skip).
	var npcSum tools.NPCSummarizer
	var loreSum tools.LoreSummarizer
	var memSum tools.MemoriseSummarizer
	if summarizer != nil {
		adapter := summarizerAdapter{s: summarizer}
		npcSum = adapter
		loreSum = adapter
		memSum = adapter
	}
	fileTools := usecase.NewFileToolset(fs, log, slow, npcSum, loreSum, memSum)
	log.Info().Str("source", fileTools.Source()).Msg("file-backed toolset ready")
	disp := dispatcher.New(cfg, fs, gitOp, fileTools, slow, log)

	gm := usecase.NewGM(usecase.GMConfig{
		Role: llm.RoleConfig{
			Model:                   role.Model,
			MaxTokens:               role.MaxTokens,
			Temperature:             role.Temperature,
			MaxEmptyRetries:         role.MaxEmptyRetries,
			EmptyRetryTimeoutSeconds: role.EmptyRetryTimeoutSeconds,
		},
		SystemPrompt: string(systemPrompt),
		Compaction: usecase.CompactionConfig{
			ContextWindow: role.ContextWindow,
			Threshold:     role.CompactionThreshold,
			KeepRecent:    role.CompactionKeepRecent,
		},
	}, fs, llmCli, ss, fl, fileTools, summarizer, sysSt, slow, cfg.LLM.TokenTracking, cfg.LLM.IncludeInReply, log)
	// Этап 0a: wire the worldStateInvalidate hook on
	// the file-backed toolset so ArchiveDay (end_day)
	// and Leave (leave_world) drop the cached WorldState
	// in GM, forcing the next turn to rebuild index:1
	// from disk. /reload (dispatcher) calls
	// gm.InvalidateWorldState directly — it does not go
	// through the toolset.
	fileTools.SetWorldStateInvalidate(gm.InvalidateWorldState)
	if !*disableLLM {
		disp.AttachGM(gm)
		log.Info().Str("model", role.Model).Str("url", role.APIURL).Msg("gm attached")
	} else {
		log.Warn().Msg("gm disabled via --no-llm; freeform will echo + validate only")
	}

	// Build the messaging client pool. Today only Telegram is wired;
	// adding Discord means constructing another client and appending
	// it to clients — no other code in this file needs to change.
	tgClient, err := telegram.New(telegram.Config{
		Token:          cfg.Messaging.Telegram.Token,
		PollingTimeout: cfg.Messaging.Telegram.PollingTimeout,
		ParseMode:      cfg.Messaging.Telegram.ParseMode,
		AllowedUserIDs: cfg.Messaging.Telegram.AllowedUserIDs,
	}, log)
	if err != nil {
		log.Fatal().Err(err).Msg("telegram init")
	}
	pool := messaging.NewMultiClient(tgClient)

	// Register Telegram command hints so the user sees a native
	// menu when they type "/" in the chat. The set is owned by
	// the dispatcher; we just translate it into the transport
	// shape here. Best-effort: if Telegram rejects (offline,
	// token revoked) we log and keep running.
	commands := disp.Commands()
	if err := tgClient.SetCommands(context.Background(), commands); err != nil {
		log.Warn().Err(err).Msg("telegram: setMyCommands failed; native menu may be stale")
	}

	ctx, cancel := context.WithCancel(context.Background())
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		log.Info().Str("signal", sig.String()).Msg("shutdown")
		cancel()
	}()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := pool.Run(ctx); err != nil {
			log.Error().Err(err).Msg("messaging pool exited")
		}
	}()

	// auto-save: counter increments per freeform bot reply. When
	// the threshold is reached the bot commits and pushes, and
	// a separate Telegram message confirms the save.
	var (
		replyCount atomic.Int64
		autoSave   = cfg.Git.AutoSave.AfterMessages
	)
	if autoSave < 0 {
		autoSave = 0
	}

	for _, c := range pool.All() {
		c := c
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case msg, ok := <-recv(c):
					if !ok {
						return
					}
					handleIncoming(ctx, log, c, disp,
						cfg.Messaging.Telegram.ParseMode,
						cfg.LLM.IncludeInReply,
						cfg.Narrative.RulesCheckBlock,
						cfg.Messaging.Telegram.ReplyToUser,
						cfg.Narrative.CompactionNotify,
						cfg.Narrative.CompactionNotifyVerbose,
						msg)
					// Auto-save counter increments on every
					// freeform reply only — commands don't
					// count because they're usually a quick
					// /status or /me.
					if msg.Command == "" {
						if n := replyCount.Add(1); autoSave > 0 && int(n)%autoSave == 0 && gitOp != nil {
							notify := runAutoSave(ctx, log, c, gitOp, msg.ChatID, cfg.Git.VerboseSave)
							if notify != "" {
								// Auto-save notify is its own bubble
								// (ReplyToMessageID=0) so it appears
								// as a meta-event, not as a reply
								// to the player's last turn.
								if err := c.Send(ctx, messaging.OutgoingMessage{ChatID: msg.ChatID, Text: notify, ParseMode: cfg.Messaging.Telegram.ParseMode}); err != nil {
									log.Error().Err(err).Str("chat", msg.ChatID).Msg("auto-save notify failed")
								}
							}
						}
					}
				}
			}
		}()
	}

	wg.Wait()
}

// chatMu serialises handleIncoming per chatID so two messages
// from the same player — or from a Telegram + Discord client
// pointing at the same logical chat — are processed strictly
// one at a time. The map is grown on demand; load is atomic.
var chatMu sync.Map // map[chatID]*sync.Mutex

func chatLock(chatID string) *sync.Mutex {
	v, _ := chatMu.LoadOrStore(chatID, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// handleIncoming is the per-message dispatch loop. It uses streaming
// when a client supports it so the player sees the answer appear
// word by word; otherwise it falls back to a single Send.
//
// The Callbacks wired below drive two streams on the same Telegram
// message:
//
//   - OnStatus rotates the placeholder ("…") through the GM's
//     phases ("…собираю контекст", "…спрашиваю qwen2.5", ...) so
//     the player sees the bot is doing something useful, not
//     just three frozen dots.
//   - OnDelta replaces the placeholder with the LLM's text as it
//     streams. Once the first text delta arrives, status
//     rotations stop competing for the same message — the text
//     wins the rest of the way.
//   - OnTokens accumulates per-round token usage. The final
//     numbers are appended to the reply when the operator
//     enabled llm.include_in_reply.
//   - OnCompaction fires after the bot trims old conversation
//     turns. We surface a one-line notice as a SEPARATE
//     Telegram message (no reply_to threading) so it appears
//     as its own bubble rather than riding the player's
//     message thread.
//
// Per-chatID serialisation: the entire body of handleIncoming
// is wrapped in chatLock(chatID) so a second freeform
// message from the same player (or a parallel Discord client
// pointing at the same chat) waits for the first to finish
// Final. This keeps the conversation thread clean and the
// reply_to threading correct.
func handleIncoming(ctx context.Context, log zerolog.Logger, c messaging.Client, disp *dispatcher.Dispatcher, parseMode string, includeTokens, rulesCheckBlock, replyTo, compactionNotify, compactionNotifyVerbose bool, msg messaging.IncomingMessage) {
	mu := chatLock(msg.ChatID)
	mu.Lock()
	defer mu.Unlock()

	// replyToMessageID is 0 when the operator disabled threading
	// or when this transport does not carry an id. Both cases
	// collapse to "send a standalone message".
	replyToID := 0
	if replyTo {
		replyToID = msg.MessageID
	}

	// Stream-capable transports (Telegram) get a throttled edit
	// session. Others receive a single Send.
	streamer, ok := c.(interface {
		StartStream(ctx context.Context, chatID string, replyToMessageID int) (messaging.StreamSession, error)
	})
	_ = streamer
	var session messaging.StreamSession
	if ok {
		s, err := c.StartStream(ctx, msg.ChatID, replyToID)
		if err != nil {
			log.Warn().Err(err).Str("chat", msg.ChatID).Msg("stream start failed, falling back to Send")
		} else {
			session = s
		}
	}

	// stripRules applies the LLM-generated "**ВАЛИДАЦИЯ ПРАВИЛ**"
	// block filter to an LLM-emitted reply. When the operator
	// flipped narrative.rules_check_block off (production
	// default) we trim the block from every delta and from the
	// final buffer so the player never sees the self-report.
	// The strip is idempotent: the regex anchor (start of line)
	// means a stray "ВАЛИДАЦИЯ ПРАВИЛ" inside a quoted NPC line
	// is left alone. When the flag is on we pass text through
	// unchanged and the player sees the block as written.
	stripRules := func(text string) string {
		if rulesCheckBlock || text == "" {
			return text
		}
		return domain.StripRulesBlock(text)
	}

	var (
		replyBuf strings.Builder
		lastTok  usecase.TokenUsage
		tokMu    sync.Mutex
		textSeen bool
		// jsonMode is true once the first chunk looks
		// like a JSON object (response_format=json_object
		// path). When set, OnDelta suppresses intermediate
		// updates because the player should not see raw
		// JSON streaming in. We accumulate silently and
		// emit a single rendered 4-block markdown at Final
		// time (see postStreamRender below).
		jsonMode bool
	)

	// postStreamRender converts the accumulated replyBuf
	// into the 4-block markdown the player sees. For JSON
	// (jsonObject mode) it parses the model output via
	// the structured package and re-emits the canonical
	// "**диалоги и действия** / КОНТЕКСТ / БУДУЩЕЕ /
	// ВАЛИДАЦИЯ ПРАВИЛ" block layout. For legacy markdown
	// the buffer is passed through unchanged. The render
	// never strips rules — that is stripRules' job, the
	// same way it has been for months.
	postStreamRender := func() string {
		raw := replyBuf.String()
		if jsonMode {
			n, err := structured.Parse(raw)
			if err != nil {
				// Fallback: surface the raw content
				// so the player still sees something.
				// The slowlog already records the parse
				// failure for the operator.
				log.Warn().Err(err).Int("chars", len(raw)).Msg("json parse failed; sending raw")
				return raw
			}
			return n.Render()
		}
		return raw
	}

	cb := usecase.Callbacks{
		OnDelta: func(s string) error {
			textSeen = true
			replyBuf.WriteString(s)
			if !jsonMode && structured.LooksLikeJSON(replyBuf.String()) {
				// First time we see a JSON-looking
				// payload: switch to silent mode. We
				// also force a re-render of the
				// placeholder (a "…" that the player
				// was seeing becomes a single dot
				// replaced at Final time).
				jsonMode = true
				return nil
			}
			if jsonMode {
				// Silent accumulation. The Final
				// call below will deliver the
				// rendered markdown in one shot.
				return nil
			}
			if session != nil {
				return session.Append(ctx, stripRules(replyBuf.String()))
			}
			return nil
		},
		OnStatus: func(phase string, details map[string]any) {
			if textSeen || session == nil {
				return
			}
			if err := session.Append(ctx, formatStatus(phase, details)); err != nil {
				log.Warn().Err(err).Msg("status append failed")
			}
		},
		OnTokens: func(u llm.Usage) {
			tokMu.Lock()
			lastTok = usecase.TokenUsage{
				PromptTokens:     u.PromptTokens,
				CompletionTokens: u.CompletionTokens,
				TotalTokens:      u.TotalTokens,
				Source:           "usage",
			}
			tokMu.Unlock()
		},
		OnCompaction: func(r usecase.CompactionResult) {
			// Compaction notice is its own bubble (no reply_to)
			// so the player sees it as a meta-event, not as the
			// answer to their message. The handler is called
			// after stream Final so the message ordering is:
			//   1. player text + streaming answer
			//   2. 🔄 компактирую...
			if !compactionNotify {
				return
			}
			notice := usecase.DescribeCompaction(r, "narrative", compactionNotifyVerbose)
			if notice == "" {
				return
			}
			if err := c.Send(ctx, messaging.OutgoingMessage{
				ChatID:           msg.ChatID,
				Text:             notice,
				ParseMode:        parseMode,
				ReplyToMessageID: 0, // standalone
			}); err != nil {
				log.Error().Err(err).Str("chat", msg.ChatID).Msg("compaction notify send failed")
			}
		},
	}

	if err := disp.HandleStream(ctx, msg, cb); err != nil {
		log.Error().Err(err).Str("chat", msg.ChatID).Msg("dispatch error")
		if session == nil {
			_ = c.Send(ctx, messaging.OutgoingMessage{
				ChatID:           msg.ChatID,
				Text:             "⚠️ " + err.Error(),
				ParseMode:        parseMode,
				ReplyToMessageID: 0,
			})
		} else {
			_ = session.Final(ctx, "⚠️ "+err.Error())
		}
		return
	}

	final := stripRules(postStreamRender())
	tokMu.Lock()
	tok := lastTok
	tokMu.Unlock()
	if includeTokens && tok.Source != "" && tok.Source != "off" && tok.TotalTokens > 0 {
		final += "\n\n🔢 ~" + itoa(tok.TotalTokens) + " tok (" + tok.Source + ")"
	}
	if final == "" {
		if session != nil {
			_ = session.Final(ctx, "…")
		}
		return
	}
	if session != nil {
		if err := session.Final(ctx, final); err != nil {
			log.Error().Err(err).Str("chat", msg.ChatID).Msg("stream final failed, retrying via Send")
			_ = c.Send(ctx, messaging.OutgoingMessage{ChatID: msg.ChatID, Text: final, ParseMode: parseMode, ReplyToMessageID: replyToID})
		}
		return
	}
	if err := c.Send(ctx, messaging.OutgoingMessage{ChatID: msg.ChatID, Text: final, ParseMode: parseMode, ReplyToMessageID: replyToID}); err != nil {
		log.Error().Err(err).Msg("send error")
	}
}

// formatStatus turns a GM phase label and optional details into
// a short, human-readable line that replaces the "…" placeholder
// before the first text delta arrives. Intentionally light on
// detail — the player should glance once and understand the bot
// is doing something useful, not stare at a wall of diagnostics.
func formatStatus(phase string, details map[string]any) string {
	switch phase {
	case "request_received":
		return "…принял"
	case "build_context":
		if world, _ := details["world"].(string); world != "" {
			return "…собираю контекст (" + world + ")"
		}
		return "…собираю контекст"
	case "llm_request":
		if model, _ := details["model"].(string); model != "" {
			return "…спрашиваю " + model
		}
		return "…спрашиваю LLM"
	case "tool_dispatch":
		if tools, ok := details["tools"].([]string); ok && len(tools) > 0 {
			return "…применяю " + strings.Join(tools, ",")
		}
		return "…применяю инструменты"
	default:
		return "…думаю"
	}
}

// itoa avoids importing strconv for one place. The bot never logs
// numbers larger than a few thousand, so the inline loop is
// cheaper than pulling in a stdlib import.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// runAutoSave commits (and pushes if not remote_disabled) and
// returns the player-facing notification text. An empty string
// means "nothing to say" (e.g. the commit was a no-op).
func runAutoSave(ctx context.Context, log zerolog.Logger, c messaging.Client, op *gitops.Operator, chatID string, verbose bool) string {
	if op == nil {
		return ""
	}
	res, err := op.CommitAll("auto: save")
	if err != nil {
		log.Error().Err(err).Msg("auto-save commit failed")
		return "⚠️ auto-save: " + err.Error()
	}
	if res.Empty {
		return ""
	}
	body := "✅ сохранено: commit " + res.Hash
	if verbose {
		body += "\n  файлов: " + itoa(len(res.FilesChanged))
		for _, f := range res.FilesChanged {
			body += "\n  - " + f
		}
	}
	if op.RemoteDisabled() {
		return body + "\n(push пропущен: remote_disabled=true)"
	}
	if err := op.SyncRebase(); err != nil {
		body += "\n⚠️ push: " + err.Error()
	} else {
		body += "\ngit push ok."
	}
	log.Info().Str("chat", chatID).Str("hash", res.Hash).Int("files", len(res.FilesChanged)).Msg("auto-save")
	return body
}

// buildSlowlog picks between File-mode and Discard based on the
// config. The path is opened in append mode; the parent
// directory is created if missing.
func buildSlowlog(cfg *config.Config, log zerolog.Logger) (*slowlog.Logger, error) {
	if !cfg.Slowlog.Enabled {
		log.Info().Msg("slowlog disabled (config: slowlog.enabled=false)")
		return slowlog.Discard(), nil
	}
	return slowlog.File(cfg.Slowlog.File)
}

// recv type-erases the per-client Recv() channel via a tiny
// interface. Keeping it here means no extra method has to live on
// messaging.Client itself.
type receiver interface {
	Recv() <-chan messaging.IncomingMessage
}

func recv(c messaging.Client) <-chan messaging.IncomingMessage {
	if r, ok := c.(receiver); ok {
		return r.Recv()
	}
	ch := make(chan messaging.IncomingMessage)
	close(ch)
	return ch
}

var _ = fmt.Sprintf

// driverClient adapts an llm.Driver (which has both Stream
// and Close) to the narrower usecase.LLMClient interface
// (Stream only). The usecase layer is unaware of driver
// lifetimes; main.go owns Close and runs it on shutdown via
// the deferred driver.Close() in main.
//
// We keep this in main rather than in internal/adapter/llm
// because it is a one-line shim and adding a package would
// be heavier than the problem warrants.
type driverClient struct {
	driver llm.Driver
}

func (d driverClient) Stream(ctx context.Context, req llm.ChatRequest, onChunk func(llm.Chunk) error) error {
	return d.driver.Stream(ctx, req, onChunk)
}

// summarizerAdapter wraps a *usecase.Summarizer (which
// returns a struct) into the three tools summarizer
// interfaces (NPCSummarizer, LoreSummarizer,
// MemoriseSummarizer — all flat []byte). We cannot do
// this inside the usecase package because that would
// create a tools → usecase import cycle; main.go is the
// only place that knows about both layers.
type summarizerAdapter struct {
	s *usecase.Summarizer
}

func (a summarizerAdapter) SummarizeNPC(ctx context.Context, displayName, world string, yamlBody, memoriseTail []byte) ([]byte, error) {
	res, err := a.s.SummarizeNPC(ctx, displayName, world, yamlBody, memoriseTail)
	if err != nil {
		return nil, err
	}
	return res.Body, nil
}

func (a summarizerAdapter) SummarizeLore(ctx context.Context, world string, loreBody, memoriseTail, stateMD []byte) ([]byte, error) {
	res, err := a.s.SummarizeLore(ctx, world, loreBody, memoriseTail, stateMD)
	if err != nil {
		return nil, err
	}
	return res.Body, nil
}

func (a summarizerAdapter) SummarizeMemorise(ctx context.Context, world string, startDay, endDay int, fullMemorise string) ([]byte, error) {
	res, err := a.s.SummarizeMemorise(ctx, world, startDay, endDay, fullMemorise)
	if err != nil {
		return nil, err
	}
	return res.Body, nil
}
