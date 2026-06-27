// Package app is the composition root for the bot binary.
//
// Responsibilities:
//
//   - Load + validate the YAML config.
//   - Build every subsystem (slowlog, storage, git, LLM
//     driver, summarizer, file toolset, GM, dispatcher,
//     messaging pool, health server) once at boot.
//   - Wire the per-message handler
//     (internal/messaging/handler.Handler) with all of its
//     dependencies via the Options struct.
//   - Own the lifecycle: Run blocks until ctx is cancelled,
//     Shutdown releases Close()-bound resources in a defined
//     order.
//
// Why this lives in its own package rather than in cmd/bot
// directly:
//
//   - cmd/bot originally mixed composition-root wiring
//     (config + DI), transport glue (handleIncoming,
//     processOneMessage, runEventLoop, formatStatus, ...)
//     and per-message state (messageState, autoSaveState,
//     chatMu, ...). The split lets main.go stay at ~30
//     statements of "flags, New, Run, wait for shutdown".
//   - Tests can construct an App with a stub config and
//     exercise the boot sequence without touching the
//     transport layer; the handler package covers the
//     per-message path independently.
//
// The App struct holds every subsystem the bot wires at
// boot. Constructed once by New and consumed by Run /
// Shutdown. Keeping the fields here (rather than as locals
// in main) makes the wiring graph explicit at a glance and
// avoids juggling a long return tuple out of bootSubsystems.
package app

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/bestxp/narrative-ai-agent/internal/adapter/gitops"
	"github.com/bestxp/narrative-ai-agent/internal/adapter/llm"
	"github.com/bestxp/narrative-ai-agent/internal/adapter/llmclient"
	"github.com/bestxp/narrative-ai-agent/internal/adapter/storage"
	"github.com/bestxp/narrative-ai-agent/internal/adapter/summarizertools"
	"github.com/bestxp/narrative-ai-agent/internal/config"
	"github.com/bestxp/narrative-ai-agent/internal/dispatcher"
	"github.com/bestxp/narrative-ai-agent/internal/health"
	"github.com/bestxp/narrative-ai-agent/internal/healthbridge"
	"github.com/bestxp/narrative-ai-agent/internal/logging"
	"github.com/bestxp/narrative-ai-agent/internal/messaging"
	"github.com/bestxp/narrative-ai-agent/internal/messaging/handler"
	promptpkg "github.com/bestxp/narrative-ai-agent/internal/prompts"
	"github.com/bestxp/narrative-ai-agent/internal/repository/api"
	"github.com/bestxp/narrative-ai-agent/internal/usecase"
	"github.com/rs/zerolog"
)

// App owns every subsystem the bot wires at boot. It is
// constructed once by New and consumed by Run / Shutdown.
type App struct {
	cfg  *config.Config
	log  zerolog.Logger
	role config.LLMRoleConfig
	prov config.ProviderConfig
	fs   *storage.FileStore
	// driver is the underlying llm.Driver; the composition
	// root owns its Close(). The handler-facing view is
	// llmCli (llm.Driver adapted to usecase.LLMClient).
	driver     llm.Driver
	llmCli     *llmclient.Driver
	summarizer *usecase.Summarizer
	slots      summarizertools.Slots
	gm         *usecase.GM
	disp       *dispatcher.Dispatcher
	pool       *messaging.MultiClient
	clients    []messaging.Client
	hsys       *healthBridge
	gitOp      *gitops.Operator
	sysSt      *usecase.SystemState
	handler    *handler.Handler
}

// NewApp constructs an App from the given CLI-derived
// arguments. It loads the config, builds every subsystem,
// wires the handler, and seeds system_state.md with the
// active character/world from info.yaml. Returns a non-nil
// error (and a nil *App) when any subsystem fails to construct.
//
// NewApp does NOT start any background goroutines. Call
// Run to spawn the messaging pool + handler goroutines.
func NewApp(configPath, logLevel string, logPretty, disableLLM bool) (*App, error) {
	if configPath == "" {
		return nil, errors.New("app: ConfigPath is required")
	}

	cfg, log, slow, err := boot(configPath, logLevel, logPretty, disableLLM)
	if err != nil {
		return nil, err
	}

	role, prov, ok := cfg.Role(config.NarrativeRole)
	if !ok {
		return nil, errors.New("narrative role not configured")
	}

	log.Info().Strs("prompts", promptpkg.List()).Msg("bundled prompts")

	systemPrompt, snap, err := bootNarrativePrompts(cfg)
	if err != nil {
		return nil, err
	}

	driver, llmCli := buildLLMDriver(role, prov, slow, log)
	summarizer, slots := buildSummarizerPipeline(cfg, role, prov, *snap, slow, log)
	fs, repos, gitOp, gm, sysSt, disp := wireDomain(
		cfg, role, prov, systemPrompt, llmCli, summarizer, slots, slow, log, disableLLM,
	)
	tb := wireTransport(cfg, log, disp, gm, role, prov, nil, repos, gitOp, sysSt, disableLLM)

	return &App{
		cfg:        cfg,
		log:        log,
		role:       role,
		prov:       prov,
		fs:         fs,
		driver:     driver,
		llmCli:     llmCli,
		summarizer: summarizer,
		slots:      slots,
		gm:         gm,
		disp:       disp,
		pool:       tb.pool,
		clients:    tb.clients,
		hsys:       tb.hsys,
		gitOp:      gitOp,
		sysSt:      sysSt,
		handler:    tb.handler,
	}, nil
}

// transportBundle is the result of wireTransport: the
// transport-facing subsystems (messaging pool, health
// server, handler) that wireTransport builds after the
// domain layer is in place. A struct here keeps the call
// sites short without a long return tuple.
type transportBundle struct {
	pool    *messaging.MultiClient
	clients []messaging.Client
	hsys    *healthBridge
	handler *handler.Handler
}

// wireTransport builds the messaging pool + health
// server + handler, attaches the GM to the dispatcher
// when LLM is enabled, and seeds system_state.md via
// InitSessionAudit. Pure plumbing — no business logic.
//
// disableLLM comes from --no-llm (validation-only runs);
// when true the dispatcher stays in echo mode and the
// GM is never asked to drive a turn.
func wireTransport(
	cfg *config.Config,
	log zerolog.Logger,
	disp *dispatcher.Dispatcher,
	gm *usecase.GM,
	role config.LLMRoleConfig,
	prov config.ProviderConfig,
	_ *storage.FileStore,
	repos *api.Repositories,
	gitOp *gitops.Operator,
	sysSt *usecase.SystemState,
	disableLLM bool,
) *transportBundle {
	if disableLLM {
		log.Warn().Msg("gm disabled via --no-llm; freeform will echo + validate only")
	} else {
		log.Info().Str("model", role.Model).Str("url", prov.APIURL).Msg("gm attached")
		disp.AttachGM(gm)
	}

	pool, clients := buildMessagingPool(cfg, disp, log)
	hsys := startHealthServer(cfg, log, clients)

	h, err := handler.New(
		cfg, disp, log,
		handler.WithSysSt(sysSt),
		handler.WithRepos(repos),
		handler.WithGitOp(gitOp),
	)
	if err != nil {
		log.Error().Err(err).Msg("handler init failed")
	}

	h.InitSessionAudit()

	return &transportBundle{
		pool: pool, clients: clients, hsys: hsys, handler: h,
	}
}

// Run blocks until ctx is cancelled or one of the
// goroutines (messaging pool or handler) exits with an
// error. The pool drives Recv() on each transport client;
// the handler reads from those Recv() channels and
// dispatches each message through HandleOne. The two are
// independent — the pool pushes messages into channels,
// the handler consumes them — so they run as siblings and
// Run returns when either side exits.
//
// On start, MarkReady is called on the health server so the
// k8s readiness probe flips to "ready" only after the
// pool's first iteration has begun.
//
// Run does NOT call Shutdown. The caller defers Shutdown
// immediately after Run to guarantee Close()-bound resources
// are released.
func (a *App) Run(ctx context.Context) error {
	if a.hsys.srv != nil {
		a.hsys.srv.MarkReady()
	}

	var wg sync.WaitGroup
	wg.Go(func() {
		if err := a.pool.Run(ctx); err != nil {
			a.log.Error().Err(err).Msg("messaging pool exited")
		}
	})

	handlerErr := a.handler.Run(ctx, a.pool)
	if handlerErr != nil {
		a.log.Error().Err(handlerErr).Msg("handler exited")
	}

	// Wait for the pool goroutine so we do not leave a
	// dangling reader on ctx cancellation.
	wg.Wait()

	if handlerErr != nil {
		return fmt.Errorf("handler run: %w", handlerErr)
	}

	return nil
}

// Shutdown releases every Close()-bound resource that New
// acquired. Idempotent: calling it twice is safe. We log
// warnings instead of returning errors because a bot
// process about to exit cannot meaningfully act on them.
func (a *App) Shutdown(ctx context.Context) {
	if a == nil {
		return
	}

	if a.hsys != nil && a.hsys.srv != nil {
		shutCtx, shutCancel := context.WithTimeout(ctx, 5*time.Second)
		_ = a.hsys.srv.Shutdown(shutCtx)

		shutCancel()
	}

	// The driver (llm.Driver) owns pooled HTTP keep-alive
	// connections and is the only resource on the boot
	// graph with a real Close() that does work. The
	// composition root owns the lifetime; we ignore the
	// error for the same reason as the health server.
	if a.driver != nil {
		_ = a.driver.Close()
	}
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
		Mode:                    cfg.Narrative.Mode,
	}

	systemPrompt, err := promptpkg.RenderNarrative(role.SystemPromptPath, *snap)
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

// healthBridge wraps the health.HTTP server with the bridge
// reporter so startHealthServer has a single value to
// return (and Shutdown has a single handle to close).
type healthBridge struct {
	clients  []messaging.Client
	reporter *healthbridge.Reporter
	srv      *health.Server
}

// startHealthServer is the k8s livenessProbe / readinessProbe
// target. Returns a stub *healthBridge when the operator left
// health.listen_addr empty; the operator-facing log line is
// printed here (not in New) so this function owns the full
// lifecycle of the health subsystem.
func startHealthServer(cfg *config.Config, log zerolog.Logger, clients []messaging.Client) *healthBridge {
	hb := &healthBridge{clients: clients, reporter: healthbridge.NewReporter(clients)}

	if cfg.Health.ListenAddr == "" {
		log.Info().Msg("health server disabled (config: health.listen_addr is empty)")

		return hb
	}

	srv := health.New(cfg.Health.ListenAddr, hb.reporter)
	if err := srv.Start(); err != nil {
		// We surface the bind error but keep the process
		// alive: the rest of the bot is still usable
		// without the probe target, and the operator may
		// not have set up the k8s sidecar yet.
		log.Error().Err(err).Str("addr", cfg.Health.ListenAddr).Msg("health server bind failed; continuing without it")

		return hb
	}

	hb.srv = srv
	log.Info().Str("addr", srv.Addr()).Msg("health server ready")

	return hb
}

// Log returns a pointer to the structured logger wired at
// boot. The pointer return type matters: zerolog.Logger's
// chaining methods (Error, Info, ...) have pointer
// receivers, so callers need a pointer to call them. Used
// by main() to surface a "event loop exited" error line
// after Run returns. Production code inside internal/*
// receives its own scoped logger; this accessor is for
// the main() error path only.
func (a *App) Log() *zerolog.Logger {
	return &a.log
}
