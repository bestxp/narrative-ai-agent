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

	cu := usecase.NewCharacterUpdate(fs, log, slow)
	disp := dispatcher.New(cfg, fs, gitOp, cu, slow, log)

	// Wire the GM. We always construct the LLM client; the no-llm
	// flag is a "dry run" toggle for environments where the LLM is
	// not yet reachable.
	role, ok := cfg.Role(config.NarrativeRole)
	if !ok {
		log.Fatal().Str("role", config.NarrativeRole).Msg("narrative role not configured")
	}
	systemPrompt, err := os.ReadFile(role.SystemPromptPath)
	if err != nil {
		log.Fatal().Err(err).Str("path", role.SystemPromptPath).Msg("read system prompt")
	}
	llmCli := llm.New(llm.RoleConfig{
		APIURL:                role.APIURL,
		APIKey:                role.APIKey,
		Model:                 role.Model,
		MaxTokens:             role.MaxTokens,
		Temperature:           role.Temperature,
		RequestTimeoutSeconds: role.RequestTimeoutSeconds,
	}, log)
	ss := usecase.NewSessionStartWithLogger(fs, log)
	mt := usecase.NewMaintenanceWithLogger(fs, log)
	fl := usecase.NewFirstLaunchWithLogger(fs, log)
	npcm := usecase.NewNPCManagerWithLogger(fs, log)
	wt := usecase.NewWorldTransitionWithLogger(fs, log)
	gm := usecase.NewGM(usecase.GMConfig{
		Role:         llm.RoleConfig{Model: role.Model, MaxTokens: role.MaxTokens, Temperature: role.Temperature},
		SystemPrompt: string(systemPrompt),
	}, fs, llmCli, ss, mt, fl, npcm, wt, cu, slow, cfg.LLM.TokenTracking, cfg.LLM.IncludeInReply, log)
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
					handleIncoming(ctx, log, c, disp, cfg.Messaging.Telegram.ParseMode, cfg.LLM.IncludeInReply, cfg.Narrative.RulesCheckBlock, msg)
					// Auto-save counter increments on every
					// freeform reply only — commands don't
					// count because they're usually a quick
					// /status or /me.
					if msg.Command == "" {
						if n := replyCount.Add(1); autoSave > 0 && int(n)%autoSave == 0 && gitOp != nil {
							notify := runAutoSave(ctx, log, c, gitOp, msg.ChatID, cfg.Git.VerboseSave)
							if notify != "" {
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
func handleIncoming(ctx context.Context, log zerolog.Logger, c messaging.Client, disp *dispatcher.Dispatcher, parseMode string, includeTokens, rulesCheckBlock bool, msg messaging.IncomingMessage) {
	streamer, ok := c.(interface {
		StartStream(ctx context.Context, chatID string) (messaging.StreamSession, error)
	})
	var session messaging.StreamSession
	if ok {
		s, err := streamer.StartStream(ctx, msg.ChatID)
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
	}

	if err := disp.HandleStream(ctx, msg, cb); err != nil {
		log.Error().Err(err).Str("chat", msg.ChatID).Msg("dispatch error")
		if session == nil {
			_ = c.Send(ctx, messaging.OutgoingMessage{ChatID: msg.ChatID, Text: "⚠️ " + err.Error(), ParseMode: parseMode})
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
			_ = c.Send(ctx, messaging.OutgoingMessage{ChatID: msg.ChatID, Text: final, ParseMode: parseMode})
		}
		return
	}
	if err := c.Send(ctx, messaging.OutgoingMessage{ChatID: msg.ChatID, Text: final, ParseMode: parseMode}); err != nil {
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
