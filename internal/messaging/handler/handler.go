package handler

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/bestxp/narrative-ai-agent/internal/adapter/gitops"
	"github.com/bestxp/narrative-ai-agent/internal/adapter/llm"
	"github.com/bestxp/narrative-ai-agent/internal/config"
	"github.com/bestxp/narrative-ai-agent/internal/dispatcher"
	"github.com/bestxp/narrative-ai-agent/internal/domain"
	"github.com/bestxp/narrative-ai-agent/internal/messaging"
	"github.com/bestxp/narrative-ai-agent/internal/repository/api"
	"github.com/bestxp/narrative-ai-agent/internal/structured"
	"github.com/bestxp/narrative-ai-agent/internal/usecase"
	"github.com/rs/zerolog"
)

// Opt configures an optional dependency of Handler.
// The three required dependencies (Config, Dispatcher, Log)
// stay positional in New so the call site reads top-down;
// everything else is opt-in.
type Opt func(*Handler)

// WithSysSt wires the system_state.md writer used for
// per-session audit rows. nil disables audit writes
// (used in tests and in the health-probe binary).
func WithSysSt(s *usecase.SystemState) Opt {
	return func(h *Handler) { h.sysSt = s }
}

// WithRepos wires the repository layer. Only Info is
// read at boot to seed the audit row. nil disables
// audit-row seeding (used in tests).
func WithRepos(r *api.Repositories) Opt {
	return func(h *Handler) { h.repos = r }
}

// WithGitOp wires the auto-save operator. nil disables
// auto-save (used in tests where the operator would
// touch the git repo).
func WithGitOp(op *gitops.Operator) Opt {
	return func(h *Handler) { h.gitOp = op }
}

// Handler is the per-message dispatch pipeline. Construct
// via New and call Run(ctx) for the event loop or HandleOne
// to process a single message synchronously (tests).
type Handler struct {
	cfg    *config.Config
	log    zerolog.Logger
	disp   *dispatcher.Dispatcher
	sysSt  *usecase.SystemState
	repos  *api.Repositories
	gitOp  *gitops.Operator
	muPool *ChatMutexPool
	auto   *AutoSaveState
}

// New builds a Handler. Config, Dispatcher and Log are
// required; sysSt, repos and gitOp are optional via the
// functional options above.
func New(cfg *config.Config, disp *dispatcher.Dispatcher, log zerolog.Logger, opts ...Opt) (*Handler, error) {
	if cfg == nil {
		return nil, errors.New("handler: Config is required")
	}

	if disp == nil {
		return nil, errors.New("handler: Dispatcher is required")
	}

	if log.GetLevel() == zerolog.Disabled {
		return nil, errors.New("handler: Log must not be disabled (use a level-configured logger)")
	}

	h := &Handler{
		cfg:    cfg,
		log:    log,
		disp:   disp,
		muPool: NewChatMutexPool(),
	}

	for _, opt := range opts {
		opt(h)
	}

	// sysSt may have been wired by WithSysSt; the auto-save
	// counter needs it for system_state.md bookkeeping.
	h.auto = NewAutoSaveState(cfg, h.sysSt)

	return h, nil
}

// Run starts the per-client goroutine fan-out. Each transport
// client in pool gets one goroutine; the goroutine reads
// from the client's Recv() channel and dispatches each
// message through HandleOne. Returns when ctx is cancelled
// or the first client exits with an error.
//
// pool.All() is used as the iteration source — the pool's
// Run() is called separately (e.g. by app.App) so that the
// Run here only handles the per-message inner loop.
func (h *Handler) Run(ctx context.Context, pool *messaging.MultiClient) error {
	if pool == nil {
		<-ctx.Done()

		return ctx.Err()
	}

	var wg sync.WaitGroup

	for _, c := range pool.All() {
		wg.Add(1)

		go func(c messaging.Client) {
			defer wg.Done()

			for {
				select {
				case <-ctx.Done():
					return
				case msg, ok := <-recv(c):
					if !ok {
						return
					}

					h.HandleOne(ctx, c, msg)
				}
			}
		}(c)
	}

	wg.Wait()

	return nil
}

// HandleOne is the per-message orchestrator. It locks the
// per-chat mutex, builds the per-message state, dispatches
// via the streaming pipeline, then runs the auto-save gate.
// Exposed so tests can drive a single message through the
// pipeline without standing up the full pool.
func (h *Handler) HandleOne(ctx context.Context, c messaging.Client, msg messaging.IncomingMessage) {
	mu := h.muPool.Lock(msg.ChatID)
	mu.Lock()
	defer mu.Unlock()

	h.bumpSessionAudit(msg.ChatID, msg.Command == "")

	parseMode, replyTo := perMessageConfig(h.cfg, c)
	replyToID := pickReplyToID(msg, replyTo)

	state := newMessageState(h.log, h.cfg.LLM.IncludeInReply)
	callbacks := h.buildCallbacks(ctx, state, h.cfg.Narrative.CompactionNotify, h.cfg.Narrative.CompactionNotifyVerbose)

	if err := h.dispatchOrFallback(ctx, c, state, callbacks, msg, replyToID, parseMode); err != nil {
		// dispatchOrFallback already surfaced the error to
		// the player; release the per-chat lock so the next
		// message is not blocked.
		return
	}

	h.sendFinalReply(ctx, c, state, h.cfg.Narrative.RulesCheckBlock, msg.ChatID, parseMode, replyToID)

	// Freeform-only auto-save gate: slash-commands are not
	// counted toward the threshold because operators do not
	// want a git commit every time they run /save.
	if msg.Command != "" {
		return
	}

	h.sendAutoSaveNotify(ctx, c, msg)
}

// InitSessionAudit seeds system_state.md with the active
// character/world/chat at bot boot. info.yaml is the single
// source of truth for "who is the bot playing right now"; the
// audit row in system_state.md mirrors that.
//
// A nil sysSt or a missing info.yaml file is silently skipped
// (the bot still boots, the audit row just stays zero). This
// keeps the health-probe binary (which has no FileStore)
// from breaking if it ever runs main().
func (h *Handler) InitSessionAudit() {
	if h.sysSt == nil || h.repos == nil {
		return
	}

	info, err := h.repos.Info.Load()
	if err != nil {
		h.log.Warn().Err(err).Msg("info.yaml load failed; skipping system_state.md init")

		return
	}

	if _, err := h.sysSt.InitSession(info.ActiveCharacter, info.ActiveWorld, "", nowUTC()); err != nil {
		h.log.Warn().Err(err).Msg("system_state.md init failed")
	}
}

// dispatchOrFallback runs the streaming dispatch. On failure
// the helper sends a best-effort error message to the player.
func (h *Handler) dispatchOrFallback(
	ctx context.Context,
	c messaging.Client,
	state *messageState,
	callbacks usecase.Callbacks,
	msg messaging.IncomingMessage,
	replyToID int,
	parseMode string,
) error {
	state.session = startStreamSession(ctx, h.log, c, msg.ChatID, replyToID)

	if err := h.disp.HandleStream(ctx, msg, callbacks); err != nil {
		h.log.Error().Err(err).Str("chat", msg.ChatID).Msg("dispatch error")
		// Best-effort fallback: report the error to the
		// player. If this also fails, nothing more we can do.
		bestEffortSend(ctx, h.log, c, state.session, msg.ChatID, "⚠️ "+err.Error(), parseMode, 0)

		return fmt.Errorf("dispatchOrFallback: %w", err)
	}

	return nil
}

// sendFinalReply renders the accumulated buffer (via the
// closures in state) and dispatches it through session.Final
// or c.Send. The stripRules + postStreamRender + token-suffix
// logic is split into finalReplyText so the dispatch step
// stays linear.
func (h *Handler) sendFinalReply(
	ctx context.Context,
	c messaging.Client,
	state *messageState,
	rulesCheckBlock bool,
	chatID string,
	parseMode string,
	replyToID int,
) {
	final := finalReplyText(state, rulesCheckBlock)

	// The sections were already streamed via Append; Final just
	// needs to close the stream with the last rendered text (or
	// the full RenderAny output if nothing was sent).
	if state.lastSentRender != "" {
		final = state.lastSentRender
	}

	if state.session == nil {
		if err := c.Send(ctx, messaging.OutgoingMessage{
			ChatID:           chatID,
			Text:             final,
			ParseMode:        parseMode,
			ReplyToMessageID: replyToID,
		}); err != nil {
			h.log.Error().Err(err).Msg("send error")
		}

		return
	}

	if err := state.session.Final(ctx, final); err != nil {
		h.log.Error().Err(err).Str("chat", chatID).Msg("stream final failed, retrying via Send")
		bestEffortSend(ctx, h.log, c, nil, chatID, final, parseMode, replyToID)
	}
}

// bumpSessionAudit is the per-message system_state.md writer.
// It is best-effort: a failure logs and returns (the player
// reply must not be blocked by a missing audit write).
func (h *Handler) bumpSessionAudit(chatID string, isFreeform bool) {
	if h.sysSt == nil {
		return
	}

	if err := h.sysSt.BumpSession(isFreeform, nowUTC()); err != nil {
		h.log.Warn().Err(err).Str("chat", chatID).Msg("system_state.md bump failed")
	}
}

// buildCallbacks wires the four usecase.Callbacks closures
// into a single struct. The closures share state by pointer
// so the same mutex protects both token and json-mode writes.
func (h *Handler) buildCallbacks(
	ctx context.Context,
	state *messageState,
	compactionNotify, compactionNotifyVerbose bool,
) usecase.Callbacks {
	return usecase.Callbacks{
		OnStatus: func(phase string, details map[string]any) {
			if state.session == nil {
				return
			}

			if err := state.session.Append(ctx, formatStatus(phase, details)); err != nil {
				h.log.Warn().Err(err).Msg("status append failed")
			}
		},
		OnDelta: func(s string) error {
			state.textSeen = true
			state.replyBuf.WriteString(s)

			if state.session != nil {
				state.streamPlainSections(ctx, h.log)
			}

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

			handleCompactionNotice(ctx, h.log, state, compactionNotifyVerbose, r)
		},
	}
}

// sendAutoSaveNotify runs the auto-save gate and pushes
// the notification. The empty-notify short-circuit (no save
// ran) returns early without sending anything.
func (h *Handler) sendAutoSaveNotify(ctx context.Context, c messaging.Client, msg messaging.IncomingMessage) {
	notify := h.auto.MaybeAutoSave(ctx, h.log, h.gitOp, msg.ChatID, h.cfg.Git.VerboseSave)
	if notify == "" {
		return
	}

	notifyPM := h.cfg.Messaging.Telegram.ParseMode
	if c.Name() == "vk" || c.Name() == "wschat" {
		notifyPM = ""
	}

	if err := c.Send(ctx, messaging.OutgoingMessage{ChatID: msg.ChatID, Text: notify, ParseMode: notifyPM}); err != nil {
		h.log.Error().Err(err).Str("chat", msg.ChatID).Msg("auto-save notify failed")
	}
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

// pickReplyToID returns msg.MessageID when replyTo is true,
// 0 otherwise. 0 collapses to "send a standalone message" in
// the transport layer.
func pickReplyToID(msg messaging.IncomingMessage, replyTo bool) int {
	if replyTo {
		return msg.MessageID
	}

	return 0
}

// bestEffortSend delivers a message to the chat, preferring the
// streaming session when one is alive. Failures are logged and
// swallowed — by the time bestEffortSend runs we are already on
// the error path (e.g. dispatch failed) and there is nothing
// meaningful we can do about a second failure.
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

// startStreamSession tries to start a streaming edit session
// for transports that support it. Returns nil if the
// transport reports streaming disabled, the start failed,
// or returned a nil session — the caller falls back to a
// single Send in that case.
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

	return raw + "\n\n🔢 ~" + strconv.Itoa(tok.TotalTokens) + " tok (" + tok.Source + ")"
}

// handleCompactionNotice formats the compaction result and
// sends it as a streaming edit. The verbose flag adds the
// before/after/dropped breakdown.
func handleCompactionNotice(
	ctx context.Context,
	log zerolog.Logger,
	state *messageState,
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

// messageState is the per-message mutable state extracted
// from HandleOne so that helper functions (token tracking,
// streaming session, render) share a single struct rather
// than passing six parameters each.
type messageState struct {
	log              zerolog.Logger
	replyBuf         strings.Builder
	lastTok          usecase.TokenUsage
	tokMu            sync.Mutex
	textSeen         bool
	lastSentRender   string
	stripRules       func(string) string
	postStreamRender func() string
	session          messaging.StreamSession
}

// newMessageState wires the closures that depend on the
// state value. The closures close over `state` so they
// have to be assigned after the struct exists.
func newMessageState(log zerolog.Logger, includeTokens bool) *messageState {
	s := &messageState{log: log, lastSentRender: ""}

	s.stripRules = func(text string) string {
		if !includeTokens || text == "" {
			return text
		}

		return domain.StripRulesBlock(text)
	}

	s.postStreamRender = func() string {
		return structured.RenderAny(s.replyBuf.String())
	}

	return s
}

// streamPlainSections parses the current reply buffer and sends
// any newly-completed sections to the player via Append. Called
// from OnDelta on every text fragment in plain mode so the
// player sees sections appear one by one rather than waiting
// for the full response.
func (s *messageState) streamPlainSections(ctx context.Context, log zerolog.Logger) {
	raw := s.replyBuf.String()

	// Use the partial renderer during the live stream so the user
	// sees only the sections that have actually arrived. Empty
	// placeholder sections are suppressed until content is produced.
	rendered := structured.RenderAnyPartial(raw)
	if rendered == "" || rendered == s.lastSentRender {
		return
	}

	s.lastSentRender = rendered
	if err := s.session.Append(ctx, rendered); err != nil {
		log.Warn().Err(err).Msg("plain section append failed")
	}
}

// receiver type-erases the per-client Recv() channel via a
// tiny interface. Transports that expose a typed Recv()
// (telegram, vk, wschat) return the channel directly;
// transports that don't return a closed channel so the
// goroutine exits cleanly on the next ctx.Done().
type receiver interface {
	Recv() <-chan messaging.IncomingMessage
}

// recv returns the per-client incoming message channel,
// returning a closed channel for clients that do not expose
// a Recv() method (the goroutine then exits cleanly on the
// next select).
func recv(c messaging.Client) <-chan messaging.IncomingMessage {
	if r, ok := c.(receiver); ok {
		return r.Recv()
	}

	ch := make(chan messaging.IncomingMessage)
	close(ch)

	return ch
}
