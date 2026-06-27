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
	"github.com/bestxp/narrative-ai-agent/internal/adapter/storage"
	"github.com/bestxp/narrative-ai-agent/internal/config"
	"github.com/bestxp/narrative-ai-agent/internal/dispatcher"
	"github.com/bestxp/narrative-ai-agent/internal/domain"
	"github.com/bestxp/narrative-ai-agent/internal/health"
	"github.com/bestxp/narrative-ai-agent/internal/logging"
	"github.com/bestxp/narrative-ai-agent/internal/messaging"
	promptpkg "github.com/bestxp/narrative-ai-agent/internal/prompts"
	"github.com/bestxp/narrative-ai-agent/internal/repository/api"
	"github.com/bestxp/narrative-ai-agent/internal/slowlog"
	"github.com/bestxp/narrative-ai-agent/internal/structured"
	"github.com/bestxp/narrative-ai-agent/internal/usecase"
	"github.com/rs/zerolog"
)

// main wiring is intentionally dense (config + DI + signal handling);
// splitting harms readability
//
// main wires every subsystem and intentionally keeps the flow
// visible at the top.
// runtime holds every subsystem the bot wires at boot. The
// struct is built once by bootSubsystems and consumed by main()
// for the event loop. Keeping the fields here (rather than as
// local variables in main) makes the wiring graph explicit at
// a glance and lets us extract bootSubsystems without
// juggling a long return tuple.
type runtime struct {
	cfg        *config.Config
	log        zerolog.Logger
	role       config.LLMRoleConfig
	prov       config.ProviderConfig
	fs         *storage.FileStore
	driver     driverClient
	summarizer *usecase.Summarizer
	slots      summarizerAdapterSlots
	gm         *usecase.GM
	disp       *dispatcher.Dispatcher
	pool       *messaging.MultiClient
	clients    []messaging.Client
	hsys       *healthServer
	gitOp      *gitops.Operator
	sysSt      *usecase.SystemState
	autoSave   *autoSaveState
}

// bootSubsystems builds every wiring that the bot needs at
// startup: storage, git, slowlog, LLM driver, summarizer,
// toolset, GM, dispatcher, messaging pool, health server.
// The caller is responsible for shutting down whatever needs
// explicit Close (LLM driver, health server, etc.).
//
// bootSubsystems is intentionally its own function so main()
// stays at 10-ish statements of "wire the thing, run the loop,
// wait for shutdown". Extracting the per-subsystem init into
// smaller helpers (buildStorage, buildGit, buildLLMDriver,
// startHealthServer, ...) is the mechanical win; keeping
// the wiring graph in a single function preserves the
// read-top-to-bottom narrative that operators rely on
// when triaging boot failures. Splitting further would
// scatter the per-step log lines.
//
// bootSubsystems wires 9 subsystems in 7 sub-calls; the sub-calls
// (bootConfig, bootLog, bootNarrativePrompts, ...) keep the function
// readable and tested in isolation.
func bootSubsystems(cfgPath, logLevel string, logPretty, disableLLM bool) (*runtime, error) {
	cfg, err := bootConfig(cfgPath)
	if err != nil {
		return nil, err
	}

	// Build the slowlog writer first so bootLog can decide whether
	// to wrap the logger with it. slowlog itself is the File-mode
	// audit log; we need it built before any subsystem emits its
	// first log line.
	slow, slowWriter, err := buildSlowlog(cfg, logging.New(logging.Config{Level: orDefault(logLevel, "info"), Pretty: logPretty}))
	if err != nil {
		return nil, fmt.Errorf("slowlog init: %w", err)
	}

	log := bootLog(cfg, logLevel, logPretty, cfg.Slowlog.Enabled, slowWriter)

	log.Info().Str("config", cfgPath).Bool("no_llm", disableLLM).Msg("starting lazy-universe bot")

	if !gitops.IsRepo(cfg.Paths.GitWorkdir) {
		log.Warn().Str("workdir", cfg.Paths.GitWorkdir).Msg("not a git repo — commits will fail")
	}

	role, prov, ok := cfg.Role(config.NarrativeRole)
	if !ok {
		return nil, errors.New("narrative role not configured")
	}

	log.Info().Strs("prompts", promptpkg.List()).Msg("bundled prompts")

	systemPrompt, renderSnap, err := bootNarrativePrompts(cfg)
	if err != nil {
		return nil, err
	}

	driver, llmCli := buildLLMDriver(role, prov, slow, log)
	summarizer, slots := buildSummarizerPipeline(cfg, role, prov, *renderSnap, slow, log)

	fs, absData := buildStorage(cfg, log)
	fileTools, repos := buildFileToolset(fs, absData, slots, slow, log)
	gitOp := buildGit(cfg, log)
	gm, sysSt := buildGM(cfg, role, prov, systemPrompt, fs, llmCli, fileTools, repos, summarizer, slow, log)
	disp := buildDispatcher(cfg, fs, gitOp, fileTools, slow, log)

	if !disableLLM {
		disp.AttachGM(gm)
		log.Info().Str("model", role.Model).Str("url", prov.APIURL).Msg("gm attached")
	} else {
		log.Warn().Msg("gm disabled via --no-llm; freeform will echo + validate only")
	}

	pool, clients := buildMessagingPool(cfg, disp, log)
	hsys := startHealthServer(cfg, log, clients)

	autoSave := newAutoSaveState(cfg, sysSt)
	initSessionAudit(log, sysSt, repos)

	return &runtime{
		cfg:        cfg,
		log:        log,
		role:       role,
		prov:       prov,
		fs:         fs,
		driver:     driverClient{driver: driver},
		summarizer: summarizer,
		slots:      slots,
		gm:         gm,
		disp:       disp,
		pool:       pool,
		clients:    clients,
		hsys:       hsys,
		gitOp:      gitOp,
		sysSt:      sysSt,
		autoSave:   autoSave,
	}, nil
}

// bootConfig loads the YAML config. Returns nil on failure
// with a wrapped error so the caller can surface
// "config load: <reason>" without losing the original.
func bootConfig(cfgPath string) (*config.Config, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, fmt.Errorf("config load: %w", err)
	}

	return cfg, nil
}

// bootLog constructs the zerolog.Logger, optionally wrapping
// it with the slowlog writer when slowlog is enabled. The
// returned logger is the one every later subsystem uses.
func bootLog(cfg *config.Config, level string, pretty, slowlogEnabled bool, slowWriter *logging.SlowlogWriter) zerolog.Logger {
	level = orDefault(level, "info")
	log := logging.New(logging.Config{Level: level, Pretty: pretty})

	if !slowlogEnabled {
		return log
	}

	log.Info().Str("path", cfg.Slowlog.File).Msg("slowlog enabled")

	return logging.NewWithSlowlog(logging.Config{Level: level, Pretty: pretty}, slowWriter)
}

// bootNarrativePrompts returns the narrative role's system
// prompt rendered through the configured template. The
// snapshot is the single source of truth for per-role
// rendering knobs (word limit, language, compaction
// notifications).
func bootNarrativePrompts(cfg *config.Config) (string, *promptpkg.NarrativeConfigSnapshot, error) {
	role, _, ok := cfg.Role(config.NarrativeRole)
	if !ok {
		return "", nil, errors.New("narrative role not configured")
	}

	snap := &promptpkg.NarrativeConfigSnapshot{
		WordLimit:               cfg.Narrative.WordLimit,
		Language:                cfg.Narrative.Language,
		RulesCheckBlock:         cfg.Narrative.RulesCheckBlock,
		CompactionNotify:        cfg.Narrative.CompactionNotify,
		CompactionNotifyVerbose: cfg.Narrative.CompactionNotifyVerbose,
	}

	systemPrompt, err := renderNarrativePrompt(role.SystemPromptPath, *snap)
	if err != nil {
		return "", nil, fmt.Errorf("read system prompt: %w", err)
	}

	return systemPrompt, snap, nil
}

// orDefault returns def when s is empty. Used by bootLog to
// default the verbosity when the operator leaves the flag
// empty.
func orDefault(s, def string) string {
	if s == "" {
		return def
	}

	return s
}

// buildSummarizerPipeline is the summarizer slot assembly
// line. It builds the cheap LLM-side summarizer (or returns
// nil when the operator chose to skip the compaction path)
// and the four summarizerAdapterSlots that the file toolset
// consumes (NPC, Lore, Chronicle, CharacterMemory). All four
// slots share the same underlying *usecase.Summarizer; the
// adapter is what makes the API surface match the per-tool
// type.
func buildSummarizerPipeline(
	cfg *config.Config,
	role config.LLMRoleConfig,
	prov config.ProviderConfig,
	snap promptpkg.NarrativeConfigSnapshot,
	slow *slowlog.Logger,
	log zerolog.Logger,
) (*usecase.Summarizer, summarizerAdapterSlots) {
	sum := buildSummarizer(cfg, role, prov, snap, slow, log)

	return sum, buildSummarizerSlots(sum)
}

// startHealthServer is the k8s livenessProbe / readinessProbe
// target. Returns a stub *healthServer when the operator left
// health.listen_addr empty; the operator-facing log line is
// printed here (not in bootSubsystems) so this function owns
// the full lifecycle of the health subsystem.
func startHealthServer(cfg *config.Config, log zerolog.Logger, clients []messaging.Client) *healthServer {
	hsys := &healthServer{clients: clients}

	if cfg.Health.ListenAddr == "" {
		log.Info().Msg("health server disabled (config: health.listen_addr is empty)")

		return hsys
	}

	srv := health.New(cfg.Health.ListenAddr, hsys)
	if err := srv.Start(); err != nil {
		// We surface the bind error but keep the process alive:
		// the rest of the bot is still usable without the
		// probe target, and the operator may not have set up
		// the k8s sidecar yet.
		log.Error().Err(err).Str("addr", cfg.Health.ListenAddr).Msg("health server bind failed; continuing without it")

		return hsys
	}

	hsys.srv = srv
	log.Info().Str("addr", srv.Addr()).Msg("health server ready")

	return hsys
}

// shutdownRuntime releases every Close()-bound resource that
// bootSubsystems acquired. Idempotent: calling it twice is safe.
// We log warnings instead of returning errors because a bot
// process about to exit cannot meaningfully act on them.
func shutdownRuntime(rt *runtime) {
	if rt == nil {
		return
	}

	if rt.hsys != nil && rt.hsys.srv != nil {
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = rt.hsys.srv.Shutdown(shutCtx)

		shutCancel()
	}
}

func main() {
	cfgPath := flag.String("config", "config.yaml", "path to config.yaml")
	logLevel := flag.String("log-level", "", "override log level (trace/debug/info/warn/error)")

	prettyLog := flag.Bool("log-pretty", false, "human-friendly console writer")
	disableLLM := flag.Bool("no-llm", false, "run without LLM (echo + validation only)")

	flag.Parse()

	rt, err := bootSubsystems(*cfgPath, *logLevel, *prettyLog, *disableLLM)
	if err != nil {
		l := zerolog.New(os.Stderr)
		l.Error().Err(err).Msg("boot failed")
		os.Exit(1)
	}
	defer shutdownRuntime(rt)

	if rt.hsys.srv != nil {
		rt.hsys.srv.MarkReady()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	awaitShutdown(ctx, cancel, rt.log)

	var wg sync.WaitGroup
	wg.Go(func() {
		if err := rt.pool.Run(ctx); err != nil {
			rt.log.Error().Err(err).Msg("messaging pool exited")
		}
	})

	runEventLoop(ctx, &wg, rt.cfg, rt.log, rt.pool, rt.disp, rt.sysSt, rt.autoSave, rt.gitOp)

	wg.Wait()
}

// awaitShutdown listens for SIGINT/SIGTERM and cancels the
// context on the first signal. The function returns when the
// first signal arrives (it does not wait for cancel to
// propagate downstream). main() defers cancel() so resources
// are released as soon as the signal handler returns.
func awaitShutdown(_ context.Context, cancel context.CancelFunc, log zerolog.Logger) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigCh
		log.Info().Str("signal", sig.String()).Msg("shutdown")
		cancel()
	}()
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

// bestEffortSend delivers a message to the chat, preferring the
// streaming session when one is alive. Failures are logged and
// swallowed — by the time bestEffortSend runs we are already on
// the error path (e.g. dispatch failed) and there is nothing
// meaningful we can do about a second failure. The function is
// intentionally tiny: per nesting.md, helpers stay under 40
// lines; this one is 13.
func bestEffortSend(
	ctx context.Context,
	log zerolog.Logger,
	c messaging.Client,
	session messaging.StreamSession,
	chatID, text, parseMode string,
	replyToMessageID int,
) {
	if session != nil {
		if err := session.Final(ctx, text); err != nil {
			log.Warn().Err(err).Str("chat", chatID).Msg("bestEffortSend session.Final failed")
		}

		return
	}

	if err := c.Send(ctx, messaging.OutgoingMessage{
		ChatID: chatID, Text: text, ParseMode: parseMode, ReplyToMessageID: replyToMessageID,
	}); err != nil {
		log.Warn().Err(err).Str("chat", chatID).Msg("bestEffortSend Send failed")
	}
}

// handleIncoming is the per-message dispatch loop. It uses streaming
// when a client supports it so the player sees the answer appear
// word by word; otherwise it falls back to a single Send.
//
// handleIncoming itself is the orchestrator: the per-message
// state (mutex, streaming session, callbacks, final-reply render)
// lives in dedicated helpers. Each helper stays under the
// 40-line nesting.md limit while the per-message flow remains
// a single read top-to-bottom.
//
// Per-chatID serialisation: the entire body of handleIncoming
// is wrapped in chatLock(chatID) so a second freeform
// message from the same player (or a parallel Discord client
// pointing at the same chat) waits for the first to finish
// Final. This keeps the conversation thread clean and the
// reply_to threading correct.
func handleIncoming(
	ctx context.Context,
	log zerolog.Logger,
	c messaging.Client,
	disp *dispatcher.Dispatcher,
	sysSt *usecase.SystemState,
	parseMode string,
	includeTokens, rulesCheckBlock, replyTo, compactionNotify, compactionNotifyVerbose bool,
	msg messaging.IncomingMessage,
) {
	mu := chatLock(msg.ChatID)
	mu.Lock()
	defer mu.Unlock()

	bumpSessionAudit(log, sysSt, msg.ChatID, msg.Command == "")

	replyToID := pickReplyToID(msg, replyTo)

	state := newMessageState(log, includeTokens)
	callbacks := buildMessageCallbacks(ctx, log, state, msg.ChatID, compactionNotify, compactionNotifyVerbose)

	if err := dispatchOrFallback(ctx, log, c, disp, sysSt, state, callbacks, msg, replyToID, parseMode); err != nil {
		// dispatchOrFallback already reported the error to the
		// player; just return so the per-message lock releases.
		return
	}

	sendFinalReply(ctx, log, c, state, rulesCheckBlock, msg.ChatID, parseMode, replyToID)
}

// pickReplyToID returns msg.MessageID when replyTo is true,
// 0 otherwise. 0 collapses to "send a standalone message" in
// the transport layer.
func pickReplyToID(msg messaging.IncomingMessage, replyTo bool) int {
	if replyTo {
		return msg.MessageID
	}

	return 0
}

// messageState is the per-message mutable state extracted
// from handleIncoming so that helper functions (token tracking,
// streaming session, render) share a single struct rather
// than passing six parameters each.
type messageState struct {
	log              zerolog.Logger
	replyBuf         strings.Builder
	lastTok          usecase.TokenUsage
	tokMu            sync.Mutex
	textSeen         bool
	jsonMode         bool
	stripRules       func(string) string
	postStreamRender func() string
	session          messaging.StreamSession
}

// newMessageState wires the closures that depend on the
// state value. The closures close over `state` so they
// have to be assigned after the struct exists.
func newMessageState(log zerolog.Logger, includeTokens bool) *messageState {
	s := &messageState{log: log}

	s.stripRules = func(text string) string {
		if !includeTokens || text == "" {
			return text
		}
		// includeTokens is the rulesCheckBlock flag in this
		// scope (it gates the strip behaviour, not the token
		// suffix which is applied in finalReplyText).
		return domain.StripRulesBlock(text)
	}

	s.postStreamRender = func() string {
		raw := structured.StripThinkingTags(s.replyBuf.String())
		if s.jsonMode {
			n, err := structured.Parse(raw)
			if err != nil {
				// Fallback: surface the raw content so the
				// player still sees something. The slowlog
				// already records the parse failure.
				s.log.Warn().Err(err).Int("chars", len(raw)).Msg("json parse failed; sending raw")

				return raw
			}

			return n.Render()
		}

		return raw
	}

	return s
}

// finalReplyText applies the rules-strip + JSON-render + the
// optional token-count suffix to the rendered buffer. The
// token suffix is gated by the rulesCheckBlock flag.
func finalReplyText(state *messageState, rulesCheckBlock bool) string {
	raw := state.stripRules(state.postStreamRender())
	if raw == "" {
		return ""
	}

	state.tokMu.Lock()
	tok := state.lastTok
	state.tokMu.Unlock()

	if !rulesCheckBlock || tok.Source == "" || tok.Source == "off" || tok.TotalTokens <= 0 {
		return raw
	}

	return raw + "\n\n🔢 ~" + itoa(tok.TotalTokens) + " tok (" + tok.Source + ")"
}

// startStreamSession tries to start a streaming edit session
// for transports that support it. Returns nil if the
// transport reports streaming disabled, the start failed,
// or returned a nil session — the caller falls back to a
// single Send in that case.
//
// streaming is opt-in per transport; messaging.Client returns the interface
//
//nolint:ireturn // messaging.Client already exposes StreamSession; no concrete type to return
func startStreamSession(
	ctx context.Context,
	log zerolog.Logger,
	c messaging.Client,
	chatID string,
	replyToMessageID int,
) messaging.StreamSession {
	s, err := c.StartStream(ctx, chatID, replyToMessageID)
	switch {
	case errors.Is(err, messaging.ErrStreamingDisabled):
		log.Debug().Str("chat", chatID).Msg("stream disabled, using Send")
	case err != nil:
		log.Warn().Err(err).Str("chat", chatID).Msg("stream start failed, falling back to Send")
	default:
		return s
	}

	return nil
}

// dispatchOrFallback runs the streaming dispatch. On failure
// the helper sends a best-effort error message to the player.
// The streaming-session setup lives in startStreamSession;
// the per-stream callbacks are built once in
// buildMessageCallbacks.
func dispatchOrFallback(
	ctx context.Context,
	log zerolog.Logger,
	c messaging.Client,
	disp *dispatcher.Dispatcher,
	_ *usecase.SystemState,
	state *messageState,
	callbacks usecase.Callbacks,
	msg messaging.IncomingMessage,
	replyToID int,
	parseMode string,
) error {
	state.session = startStreamSession(ctx, log, c, msg.ChatID, replyToID)

	if err := disp.HandleStream(ctx, msg, callbacks); err != nil {
		log.Error().Err(err).Str("chat", msg.ChatID).Msg("dispatch error")
		// Best-effort fallback: report the error to the
		// player. If this also fails, nothing more we can do.
		bestEffortSend(ctx, log, c, state.session, msg.ChatID, "⚠️ "+err.Error(), parseMode, 0)

		return fmt.Errorf("dispatchOrFallback: %w", err)
	}

	return nil
}

// sendFinalReply renders the accumulated buffer (via the
// closures in state) and dispatches it through session.Final
// or c.Send. The stripRules + postStreamRender + token-suffix
// logic is split into finalReplyText so the dispatch step
// stays linear.
func sendFinalReply(
	ctx context.Context,
	log zerolog.Logger,
	c messaging.Client,
	state *messageState,
	rulesCheckBlock bool,
	chatID string,
	parseMode string,
	replyToID int,
) {
	final := finalReplyText(state, rulesCheckBlock)
	if state.session == nil {
		if err := c.Send(ctx, messaging.OutgoingMessage{
			ChatID:           chatID,
			Text:             final,
			ParseMode:        parseMode,
			ReplyToMessageID: replyToID,
		}); err != nil {
			log.Error().Err(err).Msg("send error")
		}

		return
	}

	if err := state.session.Final(ctx, final); err != nil {
		log.Error().Err(err).Str("chat", chatID).Msg("stream final failed, retrying via Send")
		bestEffortSend(ctx, log, c, nil, chatID, final, parseMode, replyToID)
	}
}

// buildMessageCallbacks wires the four usecase.Callbacks
// closures into a single struct. The closures share state
// by pointer so the same mutex protects both token and
// json-mode writes.
func buildMessageCallbacks(
	ctx context.Context,
	log zerolog.Logger,
	state *messageState,
	_ string,
	compactionNotify, compactionNotifyVerbose bool,
) usecase.Callbacks {
	return usecase.Callbacks{
		OnStatus: func(phase string, details map[string]any) {
			if state.session == nil {
				return
			}

			if err := state.session.Append(ctx, formatStatus(phase, details)); err != nil {
				log.Warn().Err(err).Msg("status append failed")
			}
		},
		OnDelta: func(s string) error {
			state.textSeen = true
			state.replyBuf.WriteString(s)

			return nil
		},
		OnTokens: func(u llm.Usage) {
			state.tokMu.Lock()
			state.lastTok = usecase.TokenUsage{
				PromptTokens:     u.PromptTokens,
				CompletionTokens: u.CompletionTokens,
				TotalTokens:      u.TotalTokens,
				Source:           "usage",
			}
			state.tokMu.Unlock()
		},
		OnCompaction: func(r usecase.CompactionResult) {
			if !compactionNotify {
				return
			}

			handleCompactionNotice(ctx, log, state, "", compactionNotifyVerbose, r)
		},
	}
}

// handleCompactionNotice formats the compaction result and
// sends it as a streaming edit. The verbose flag adds the
// before/after/dropped breakdown.
func handleCompactionNotice(
	ctx context.Context,
	log zerolog.Logger,
	state *messageState,
	_ string,
	verbose bool,
	r usecase.CompactionResult,
) {
	detail := ""
	if verbose {
		detail = fmt.Sprintf("\n  до:    %d tok\n  после: %d tok\n  дроп:  %d turn(s)",
			r.BeforeTokens, r.AfterTokens, r.DroppedTurns)
	}

	notice := fmt.Sprintf("🔄 компактирую историю (%d → %d tok, −%d хода)%s",
		r.BeforeTokens, r.AfterTokens, r.DroppedTurns, detail)
	if err := state.session.Append(ctx, notice); err != nil {
		log.Warn().Err(err).Msg("compaction notify append failed")
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

// initSessionAudit seeds system_state.md with the active
// character/world/chat at bot boot. info.yaml is the single
// source of truth for "who is the bot playing right now"; the
// audit row in system_state.md mirrors that.
//
// A nil sysSt or a missing info.yaml file is silently skipped
// (the bot still boots, the audit row just stays zero). This
// keeps the health-probe binary (which has no FileStore)
// from breaking if it ever runs main().
//
// The helper exists to keep main() flat — every other branch
// in main deals with subsystem wiring, not state-file I/O.
func initSessionAudit(log zerolog.Logger, sysSt *usecase.SystemState, repos *api.Repositories) {
	if sysSt == nil {
		return
	}

	info, err := repos.Info.Load()
	if err != nil {
		log.Warn().Err(err).Msg("info.yaml load failed; skipping system_state.md init")

		return
	}

	if _, err := sysSt.InitSession(info.ActiveCharacter, info.ActiveWorld, "", time.Now().UTC()); err != nil {
		log.Warn().Err(err).Msg("system_state.md init failed")
	}
}

// runEventLoop spawns one goroutine per transport client. Each
// goroutine reads from its client channel and dispatches the
// message through handleIncoming, then runs the auto-save
// gate. The function is its own scope so main() stays flat —
// every main() branch after runEventLoop is "wait for shutdown,
// close the health server". Extracting the inner loop also
// keeps handleIncoming flat (it is per-message, not per-client).
func runEventLoop(
	ctx context.Context,
	wg *sync.WaitGroup,
	cfg *config.Config,
	log zerolog.Logger,
	pool *messaging.MultiClient,
	disp *dispatcher.Dispatcher,
	sysSt *usecase.SystemState,
	autoSave *autoSaveState,
	gitOp *gitops.Operator,
) {
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

					processOneMessage(ctx, cfg, log, c, disp, sysSt, autoSave, gitOp, msg)
				}
			}
		})
	}
}

// processOneMessage runs the per-message pipeline: pick parse
// mode + reply threading, hand off to handleIncoming for the
// streaming reply, and (only for freeform turns) run the
// auto-save gate. The parse-mode / reply-mode split lives in
// perMessageConfig and the auto-save notify lives in
// sendAutoSaveNotify so this function stays under nesting.md's
// 40-line limit while keeping the linear top-to-bottom story.
func processOneMessage(
	ctx context.Context,
	cfg *config.Config,
	log zerolog.Logger,
	c messaging.Client,
	disp *dispatcher.Dispatcher,
	sysSt *usecase.SystemState,
	autoSave *autoSaveState,
	gitOp *gitops.Operator,
	msg messaging.IncomingMessage,
) {
	pm, rt := perMessageConfig(cfg, c)

	handleIncoming(ctx, log, c, disp, sysSt,
		pm,
		cfg.LLM.IncludeInReply,
		cfg.Narrative.RulesCheckBlock,
		rt,
		cfg.Narrative.CompactionNotify,
		cfg.Narrative.CompactionNotifyVerbose,
		msg)

	// Freeform-only auto-save gate: slash-commands are not
	// counted toward the threshold because operators do not
	// want a git commit every time they run /save.
	if msg.Command != "" {
		return
	}

	sendAutoSaveNotify(ctx, log, c, cfg, autoSave, gitOp, msg)
}

// perMessageConfig picks the parse mode and reply-threading
// behaviour for the current transport. Telegram renders
// MarkdownV2 (operator-friendly); VK / wschat get plain text
// so a stray underscore in a commit message does not break
// the notification.
func perMessageConfig(cfg *config.Config, c messaging.Client) (string, bool) {
	parseMode := cfg.Messaging.Telegram.ParseMode
	replyToUser := cfg.Messaging.Telegram.ReplyToUser

	if c.Name() == "vk" || c.Name() == "wschat" {
		parseMode = ""
		replyToUser = true
	}

	return parseMode, replyToUser
}

// sendAutoSaveNotify runs the auto-save gate and pushes the
// notification. The empty-notify short-circuit (no save
// ran) returns early without sending anything.
func sendAutoSaveNotify(
	ctx context.Context,
	log zerolog.Logger,
	c messaging.Client,
	cfg *config.Config,
	autoSave *autoSaveState,
	gitOp *gitops.Operator,
	msg messaging.IncomingMessage,
) {
	notify := autoSave.maybeAutoSave(ctx, log, gitOp, msg.ChatID, cfg.Git.VerboseSave)
	if notify == "" {
		return
	}

	notifyPM := cfg.Messaging.Telegram.ParseMode
	if c.Name() == "vk" || c.Name() == "wschat" {
		notifyPM = ""
	}

	if err := c.Send(ctx, messaging.OutgoingMessage{ChatID: msg.ChatID, Text: notify, ParseMode: notifyPM}); err != nil {
		log.Error().Err(err).Str("chat", msg.ChatID).Msg("auto-save notify failed")
	}
}

// bumpSessionAudit is the per-message system_state.md writer.
// It is best-effort: a failure logs and returns (the player
// reply must not be blocked by a missing audit write). The
// helper exists to keep handleIncoming flat — every other
// branch in that function deals with reply rendering, not
// state-file I/O.
func bumpSessionAudit(log zerolog.Logger, sysSt *usecase.SystemState, chatID string, isFreeform bool) {
	if sysSt == nil {
		return
	}

	if err := sysSt.BumpSession(isFreeform, time.Now().UTC()); err != nil {
		log.Warn().Err(err).Str("chat", chatID).Msg("system_state.md bump failed")
	}
}

// runAutoSave commits (and pushes if not remote_disabled) and
// returns the player-facing notification text. An empty string
// means "nothing to say" (e.g. the commit was a no-op).
//
// The sysSt argument is optional: a nil value skips the
// system_state.md bookkeeping (used by tests and by the
// health-probe binary which does not own a FileStore). Production
// always passes a non-nil sysSt.
func runAutoSave(
	ctx context.Context,
	log zerolog.Logger,
	op *gitops.Operator,
	sysSt *usecase.SystemState,
	_ string,
	verbose bool,
) string {
	if op == nil {
		return ""
	}

	res, err := op.CommitAll(ctx, "auto: save")
	if err != nil {
		log.Error().Err(err).Msg("auto-save commit failed")

		return "⚠️ auto-save: " + err.Error()
	}

	if res.Empty {
		return ""
	}

	// Record the autosave in system_state.md so the operator can
	// confirm "did the bot actually save" without trawling zerolog.
	// RecordAutosave is no-op for empty commits (the counter
	// reflects successful non-empty saves, not save attempts).
	if sysSt != nil && res.Hash != "" {
		if _, err := sysSt.RecordAutosave(res.Hash, time.Now().UTC()); err != nil {
			log.Warn().Err(err).Str("hash", res.Hash).Msg("system_state.md autosave record failed")
		}
	}

	body := buildAutoSaveNotify(res, verbose)
	if op.RemoteDisabled() {
		return body + "\n(push пропущен: remote_disabled=true)"
	}

	return appendPushStatus(ctx, log, op, res, body)
}

// buildAutoSaveNotify formats the ✅ "saved: commit <hash>" prefix
// (plus the optional per-file diff when verbose=true). The caller
// appends the push status separately so the function stays
// pure and testable.
func buildAutoSaveNotify(res gitops.CommitResult, verbose bool) string {
	var b strings.Builder

	b.WriteString("✅ сохранено: commit ")
	b.WriteString(res.Hash)

	if !verbose {
		return b.String()
	}

	b.WriteString("\n  файлов: ")
	b.WriteString(itoa(len(res.FilesChanged)))

	for _, f := range res.FilesChanged {
		b.WriteString("\n  - ")
		b.WriteString(f)
	}

	return b.String()
}

// appendPushStatus tries to push the commit to the remote and
// appends a single line describing the result. Errors are
// reported inline so the player sees "⚠️ push: <reason>"
// rather than just an empty success line.
func appendPushStatus(ctx context.Context, log zerolog.Logger, op *gitops.Operator, res gitops.CommitResult, body string) string {
	if err := op.SyncRebase(ctx); err != nil {
		return body + "\n⚠️ push: " + err.Error()
	}

	log.Info().
		Str("chat", "").
		Str("hash", res.Hash).
		Int("files", len(res.FilesChanged)).
		Msg("auto-save pushed")

	return body + "\ngit push ok."
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

	out, err := promptpkg.Render(name, data)
	if err != nil {
		return "", fmt.Errorf("render summarizer prompt %q: %w", name, err)
	}

	return out, nil
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
		out, err := promptpkg.Render("narrative.md.tmpl", data)
		if err != nil {
			return "", fmt.Errorf("render narrative prompt: %w", err)
		}

		return out, nil
	}

	body, err := os.ReadFile(overridePath)
	if err != nil {
		if os.IsNotExist(err) {
			// No override file — fall through to the
			// embedded template. The operator may
			// have set the path in config but not
			// created the file yet.
			out, err := promptpkg.Render("narrative.md.tmpl", data)
			if err != nil {
				return "", fmt.Errorf("render narrative prompt: %w", err)
			}

			return out, nil
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
