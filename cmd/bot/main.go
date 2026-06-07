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

	"narrative/internal/adapter/gitops"
	"narrative/internal/adapter/llm"
	"narrative/internal/adapter/storage"
	"narrative/internal/config"
	"narrative/internal/dispatcher"
	"narrative/internal/domain"
	"narrative/internal/logging"
	"narrative/internal/messaging"
	"narrative/internal/messaging/telegram"
	promptpkg "narrative/internal/prompts"
	"narrative/internal/slowlog"
	"narrative/internal/usecase"
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

	// One Tool bundles every concern. main.go constructs it
	// once, hands it to the dispatcher and the GM, and that's
	// the entire wiring. The previous five-concrete-objects
	// layout made it trivial to forget to wire one of them;
	// the single Tool surface fails closed.
	tools := usecase.NewFileToolset(fs, log, slow)
	log.Info().Str("source", tools.Source()).Msg("file-backed toolset ready")
	disp := dispatcher.New(cfg, fs, gitOp, tools, slow, log)

	role, ok := cfg.Role(config.NarrativeRole)
	if !ok {
		log.Fatal().Str("role", config.NarrativeRole).Msg("narrative role not configured")
	}
	log.Info().Strs("prompts", promptpkg.List()).Msg("bundled prompts")
	systemPrompt, err := promptpkg.LoadSystemPrompt(role.SystemPromptPath, "narrative.md")
	if err != nil {
		log.Fatal().Err(err).Str("path", role.SystemPromptPath).Msg("read system prompt")
	}
	llmCli := llm.New(llm.RoleConfig{
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
	ss := usecase.NewSessionStartWithLogger(fs, log)
	fl := usecase.NewFirstLaunchWithLogger(fs, log)
	sysSt := usecase.NewSystemState(fs, log, slow)

	// summarizer: three modes.
	//   1. Dedicated `summary` role in config → use it.
	//   2. No summary role, but `narrative` is wired → fallback
	//      to narrative with clamped max_tokens/temperature.
	//   3. Neither → no summariser, drop-only compaction.
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
			sumLLM := llm.New(sumRole, log)
			if _, hasSummary := cfg.Role("summary"); hasSummary {
				summarizer = usecase.NewSummarizer(sumLLM, sumRole, sumPrompt, slow, log)
			} else {
				summarizer = usecase.NewFallbackSummarizer(sumLLM, sumRole, sumPrompt, slow, log)
			}
		}
	}

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
	}, fs, llmCli, ss, fl, tools, summarizer, sysSt, slow, cfg.LLM.TokenTracking, cfg.LLM.IncludeInReply, log)
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
	)

	cb := usecase.Callbacks{
		OnDelta: func(s string) error {
			textSeen = true
			replyBuf.WriteString(s)
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

	final := stripRules(replyBuf.String())
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
