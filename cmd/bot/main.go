package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"text/template"
	"time"

	"github.com/bestxp/narrative-ai-agent/internal/adapter/gitops"
	"github.com/bestxp/narrative-ai-agent/internal/adapter/llm"
	"github.com/bestxp/narrative-ai-agent/internal/config"
	"github.com/bestxp/narrative-ai-agent/internal/dispatcher"
	"github.com/bestxp/narrative-ai-agent/internal/domain"
	"github.com/bestxp/narrative-ai-agent/internal/health"
	"github.com/bestxp/narrative-ai-agent/internal/logging"
	"github.com/bestxp/narrative-ai-agent/internal/messaging"
	promptpkg "github.com/bestxp/narrative-ai-agent/internal/prompts"
	"github.com/bestxp/narrative-ai-agent/internal/slowlog"
	"github.com/bestxp/narrative-ai-agent/internal/structured"
	"github.com/bestxp/narrative-ai-agent/internal/usecase"
	"github.com/rs/zerolog"
)

//nolint:gocognit // intentional complexity; main wiring function
func main() { //nolint:funlen // complex function; splitting would harm readability.
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

	fs, absData := buildStorage(cfg, log)

	if !gitops.IsRepo(cfg.Paths.GitWorkdir) {
		log.Warn().Str("workdir", cfg.Paths.GitWorkdir).Msg("not a git repo — commits will fail")
	}
	gitOp := buildGit(cfg, log)

	slow, slowWriter, err := buildSlowlog(cfg, log)
	if err != nil {
		log.Fatal().Err(err).Msg("slowlog init")
	}

	if cfg.Slowlog.Enabled {
		log.Info().Str("path", cfg.Slowlog.File).Msg("slowlog enabled")
		log = logging.NewWithSlowlog(logging.Config{Level: level, Pretty: *prettyLog}, slowWriter)
	}

	role, ok := cfg.Role(config.NarrativeRole)
	if !ok {
		log.Fatal().Str("role", config.NarrativeRole).Msg("narrative role not configured")
	}
	log.Info().Strs("prompts", promptpkg.List()).Msg("bundled prompts")
	renderSnap := promptpkg.NarrativeConfigSnapshot{
		WordLimit:                  cfg.Narrative.WordLimit,
		Language:                   cfg.Narrative.Language,
		RulesCheckBlock:            cfg.Narrative.RulesCheckBlock,
		IncludeSystemStateInPrompt: cfg.Narrative.IncludeSystemStateInPrompt,
		CompactionNotify:           cfg.Narrative.CompactionNotify,
		CompactionNotifyVerbose:    cfg.Narrative.CompactionNotifyVerbose,
	}
	compactionSnap := renderSnap
	systemPrompt, err := renderNarrativePrompt(role.SystemPromptPath, renderSnap)
	if err != nil {
		log.Fatal().Err(err).Str("path", role.SystemPromptPath).Msg("read system prompt")
	}

	driver, llmCli := buildLLMDriver(cfg, role, slow, log)
	defer func() { _ = driver.Close() }()

	summarizer := buildSummarizer(cfg, role, compactionSnap, slow, log)
	slots := buildSummarizerSlots(summarizer)

	fileTools, repos := buildFileToolset(fs, absData, slots, slow, log)
	gm := buildGM(cfg, role, systemPrompt, fs, llmCli, fileTools, repos, summarizer, slow, log)
	disp := buildDispatcher(cfg, fs, gitOp, fileTools, slow, log)

	if !*disableLLM {
		disp.AttachGM(gm)
		log.Info().Str("model", role.Model).Str("url", role.APIURL).Msg("gm attached")
	} else {
		log.Warn().Msg("gm disabled via --no-llm; freeform will echo + validate only")
	}

	pool, clients := buildMessagingPool(cfg, disp, log)

	// Health server: k8s livenessProbe / readinessProbe targets.
	hs := &healthServer{clients: clients}
	if cfg.Health.ListenAddr != "" {
		srv := health.New(cfg.Health.ListenAddr, hs)

		if err := srv.Start(); err != nil {
			// os.Exit will skip the deferred driver.Close() above,
			// but the health server failed to bind — there is nothing
			// to clean up yet and we want to fail fast.
			log.Error().Err(err).Str("addr", cfg.Health.ListenAddr).Msg("health server bind failed")

			_ = driver.Close()

			os.Exit(1)
		}
		hs.srv = srv
		log.Info().Str("addr", srv.Addr()).Msg("health server ready")
	} else {
		log.Info().Msg("health server disabled (config: health.listen_addr is empty)")
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
	wg.Go(func() {
		if err := pool.Run(ctx); err != nil {
			log.Error().Err(err).Msg("messaging pool exited")
		}
	})

	if hs.srv != nil {
		hs.srv.MarkReady()
	}

	autoSave := newAutoSaveState(cfg)

	for _, c := range pool.All() {
		wg.Go(func() {
			for {
				select {
				case <-ctx.Done():
					return
				case msg, ok := <-recv(c):
					if !ok {
						return
					}
					pm := cfg.Messaging.Telegram.ParseMode
					rt := cfg.Messaging.Telegram.ReplyToUser

					if c.Name() == "vk" || c.Name() == "wschat" {
						pm = ""
						rt = true
					}
					handleIncoming(ctx, log, c, disp,
						pm,
						cfg.LLM.IncludeInReply,
						cfg.Narrative.RulesCheckBlock,
						rt,
						cfg.Narrative.CompactionNotify,
						cfg.Narrative.CompactionNotifyVerbose,
						msg)

					//nolint:nestif // intentional nesting for command vs freeform dispatch
					if msg.Command == "" {
						if notify := autoSave.maybeAutoSave(ctx, log, c, gitOp, msg.ChatID, cfg.Git.VerboseSave); notify != "" {
							notifyPM := cfg.Messaging.Telegram.ParseMode
							if c.Name() == "vk" || c.Name() == "wschat" {
								notifyPM = ""
							}
							if err := c.Send(ctx, messaging.OutgoingMessage{ChatID: msg.ChatID, Text: notify, ParseMode: notifyPM}); err != nil {
								log.Error().Err(err).Str("chat", msg.ChatID).Msg("auto-save notify failed")
							}
						}
					}
				}
			}
		})
	}

	wg.Wait()

	if hs.srv != nil {
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = hs.srv.Shutdown(shutCtx)

		shutCancel()
	}
}

// chatMu serialises handleIncoming per chatID so two messages
// from the same player — or from a Telegram + Discord client
// pointing at the same logical chat — are processed strictly
// one at a time. The map is grown on demand; load is atomic.
// chatMu is the per-chat mutex cache. Concurrent messages
// from the same player — or from a Telegram + Discord client
// pointing at the same logical chat — are processed strictly
// one at a time. The map is grown on demand; load is atomic.
//
//nolint:gochecknoglobals // process-wide mutex pool keyed by chatID
var chatMu sync.Map // map[chatID]*sync.Mutex

func chatLock(chatID string) *sync.Mutex {
	v, _ := chatMu.LoadOrStore(chatID, &sync.Mutex{})

	mu, ok := v.(*sync.Mutex)
	if !ok {
		panic(fmt.Sprintf("chatMu: expected *sync.Mutex, got %T", v))
	}

	return mu
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
// reply_to threading correct. //nolint:funlen // complex function; splitting would harm readability.
//
//nolint:gocognit // intentional complexity; per-message dispatch loop //nolint:funlen // complex function; splitting would harm readability.
func handleIncoming(ctx context.Context, log zerolog.Logger, c messaging.Client, disp *dispatcher.Dispatcher, parseMode string, includeTokens, rulesCheckBlock, replyTo, compactionNotify, compactionNotifyVerbose bool, msg messaging.IncomingMessage) { //nolint:funlen // complex function; splitting would harm readability.
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
	_, ok := c.(interface {
		StartStream(ctx context.Context, chatID string, replyToMessageID int) (messaging.StreamSession, error)
	})
	var session messaging.StreamSession

	if ok {
		s, err := c.StartStream(ctx, msg.ChatID, replyToID)

		switch {
		case errors.Is(err, messaging.ErrStreamingDisabled):
			log.Debug().Str("chat", msg.ChatID).Msg("stream disabled, using Send")
		case err != nil:
			log.Warn().Err(err).Str("chat", msg.ChatID).Msg("stream start failed, falling back to Send")
		default:
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
		raw := structured.StripThinkingTags(replyBuf.String())
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

	if err := c.Send(ctx, messaging.OutgoingMessage{
		ChatID: msg.ChatID, Text: final, ParseMode: parseMode, ReplyToMessageID: replyToID,
	}); err != nil {
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
	var b strings.Builder
	b.WriteString("✅ сохранено: commit ")
	b.WriteString(res.Hash)
	if verbose {
		b.WriteString("\n  файлов: ")
		b.WriteString(itoa(len(res.FilesChanged)))

		for _, f := range res.FilesChanged {
			b.WriteString("\n  - ")
			b.WriteString(f)
		}
	}
	body := b.String()
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
//
// It returns both the slowlog.Logger (for structured events)
// and a logging.SlowlogWriter (for zerolog console→slowlog
// duplication). When slowlog is disabled both are no-ops.
func buildSlowlog(cfg *config.Config, log zerolog.Logger) (*slowlog.Logger, *logging.SlowlogWriter, error) {
	if !cfg.Slowlog.Enabled {
		log.Info().Msg("slowlog disabled (config: slowlog.enabled=false)")
		return slowlog.Discard(), logging.NewSlowlogWriter(io.Discard), nil
	}
	sl, err := slowlog.File(cfg.Slowlog.File)
	if err != nil {
		return nil, nil, fmt.Errorf("slowlog file open: %w", err)
	}
	// The SlowlogWriter wraps the same *os.File so that
	// zerolog's MultiWriter and slowlog.Logger.Write()
	// both serialize through their own mutexes — no
	// interleaving between JSON-line entries and
	// slowlog's own structured events.
	return sl, logging.NewSlowlogWriter(sl.Writer()), nil
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
	return fmt.Errorf("driver stream: %w", d.driver.Stream(ctx, req, onChunk))
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

func (a summarizerAdapter) SummarizeNPC(
	ctx context.Context,
	displayName, world string,
	yamlBody, chronicleContext []byte,
) ([]byte, error) {
	res, err := a.s.SummarizeNPC(ctx, displayName, world, yamlBody, chronicleContext)
	if err != nil {
		return nil, fmt.Errorf("summarize npc: %w", err)
	}

	return res.Body, nil
}

func (a summarizerAdapter) SummarizeLore(
	ctx context.Context,
	world string,
	loreBody, chronicleContext, stateMD []byte,
) ([]byte, error) {
	res, err := a.s.SummarizeLore(ctx, world, loreBody, chronicleContext, stateMD)
	if err != nil {
		return nil, fmt.Errorf("summarize lore: %w", err)
	}

	return res.Body, nil
}

func (a summarizerAdapter) SummarizeChronicle(
	ctx context.Context, world string, startDay, endDay int,
	fullChronicle string,
) ([]byte, error) {
	res, err := a.s.SummarizeChronicle(ctx, world, startDay, endDay, fullChronicle)
	if err != nil {
		return nil, fmt.Errorf("summarize chronicle: %w", err)
	}

	return res.Body, nil
}

func (a summarizerAdapter) SummarizeCharacterMemory(
	ctx context.Context, world, character string,
	memoryBody, chronicleTail []byte,
) ([]byte, error) {
	res, err := a.s.SummarizeCharacterMemory(ctx, world, character, memoryBody, chronicleTail)
	if err != nil {
		return nil, fmt.Errorf("summarize character memory: %w", err)
	}

	return res.Body, nil
}

// renderSummarizerPrompt loads a summarizer-side
// prompt by name and renders it with the data-bag.
// The summarizer prompts (compaction_in_place,
// end_of_day, character_memory_maintain, lore_summary,
// memorise_summary, npc_summary) are static — they
// depend only on NarrativeConfigSnapshot and not on
// per-turn context. Rendering once at startup is
// enough; the resulting string is handed to the
// summarizer's setters and re-used on every call.
func renderSummarizerPrompt(name string, snap promptpkg.NarrativeConfigSnapshot) (string, error) {
	data := promptpkg.NewPromptData(snap, promptpkg.CharacterData{}, promptpkg.WorldData{})

	return promptpkg.Render(name, data)
}

// renderNarrativePrompt loads the system prompt for
// the narrative role. Override-on-disk wins: if the
// operator configured a path to a hand-written
// .md or .md.tmpl file, it is read verbatim. If the
// file looks like a Go template (contains "{{") or
// has a .md.tmpl extension, it is rendered with the
// data-bag. Otherwise the override is returned
// as-is. When no override is configured, the
// embedded narrative.md.tmpl is rendered.
func renderNarrativePrompt(overridePath string, snap promptpkg.NarrativeConfigSnapshot) (string, error) {
	data := promptpkg.PromptData{Character: promptpkg.CharacterData{}, World: promptpkg.WorldData{}}
	data = promptpkg.NewPromptData(snap, data.Character, data.World)
	if overridePath == "" {
		return promptpkg.Render("narrative.md.tmpl", data)
	}
	body, err := os.ReadFile(overridePath)
	if err != nil {
		if os.IsNotExist(err) {
			// No override file — fall through to the
			// embedded template. The operator may
			// have set the path in config but not
			// created the file yet.
			return promptpkg.Render("narrative.md.tmpl", data)
		}

		return "", fmt.Errorf("read override %q: %w", overridePath, err)
	}
	text := strings.TrimSpace(string(body))
	// Plain markdown override (operator drops a
	// hand-written narrative.md without template
	// markers) — return as-is.
	if !strings.HasSuffix(overridePath, ".md.tmpl") && !looksLikeTemplate(text) {
		return text, nil
	}
	// Treat as a template, render with the data-bag.
	return renderFromBody(overridePath, text, data)
}

// looksLikeTemplate is a cheap sniff for "{{" markers
// that distinguishes plain markdown from Go-template
// source. False positives are harmless: a markdown file
// containing "{{" simply gets parsed as a template (and
// the missing-key check will fire if a real {{ is
// present).
func looksLikeTemplate(body string) bool {
	return strings.Contains(body, "{{")
}

// renderFromBody parses body as a Go template with the
// data-bag and returns the rendered text. The wrapper
// around the embedded template cache is intentionally
// minimal: the override path is rare, so the parse cost
// is paid once per process.
func renderFromBody(name, body string, data promptpkg.PromptData) (string, error) {
	tpl, err := template.New(name).Option("missingkey=error").Parse(body)
	if err != nil {
		return "", fmt.Errorf("parse override %q: %w", name, err)
	}
	var b strings.Builder
	if err := tpl.Execute(&b, data); err != nil {
		return "", fmt.Errorf("execute override %q: %w", name, err)
	}

	return b.String(), nil
}

// healthServer is the bridge between the messaging layer (which
// owns Health() on each client) and the health package (which
// wants a Reporter). Keeping the conversion here means neither
// package needs to import the other.
type healthServer struct {
	clients []messaging.Client
	srv     *health.Server
}

// Reports snapshots each client's Health() and converts the
// messaging.HealthState enum to the health.Status string used on
// the wire.
func (h *healthServer) Reports() []health.Report {
	out := make([]health.Report, 0, len(h.clients))
	for _, c := range h.clients {
		r := c.Health()
		var startedAt string
		if !r.StartedAt.IsZero() {
			startedAt = r.StartedAt.UTC().Format(time.RFC3339)
		}
		out = append(out, health.Report{
			Name:      health.Status(c.Name()),
			State:     health.Status(r.State),
			StartedAt: startedAt,
			Message:   r.Message,
		})
	}

	return out
}
