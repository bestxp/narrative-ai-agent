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
	"syscall"

	"github.com/rs/zerolog"

	"narrative/internal/adapter/gitops"
	"narrative/internal/adapter/llm"
	"narrative/internal/adapter/storage"
	"narrative/internal/config"
	"narrative/internal/dispatcher"
	"narrative/internal/logging"
	"narrative/internal/messaging"
	"narrative/internal/messaging/telegram"
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

	disp := dispatcher.New(cfg, fs, gitOp, log)

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
	}, fs, llmCli, ss, mt, fl, npcm, wt, log)
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
					handleIncoming(ctx, log, c, disp, gm, cfg.Messaging.Telegram.ParseMode, msg)
				}
			}
		}()
	}

	wg.Wait()
}

// handleIncoming is the per-message dispatch loop. It uses streaming
// when a client supports it so the player sees the answer appear
// word by word; otherwise it falls back to a single Send.
func handleIncoming(ctx context.Context, log zerolog.Logger, c messaging.Client, disp *dispatcher.Dispatcher, gm *usecase.GM, parseMode string, msg messaging.IncomingMessage) {
	// Stream-capable transports (Telegram) get a throttled edit
	// session. Others receive a single Send.
	streamer, ok := c.(interface {
		StartStream(ctx context.Context, chatID string) (messaging.StreamSession, error)
	})
	var session messaging.StreamSession
	if ok {
		s, err := streamer.StartStream(ctx, msg.ChatID)
		if err == nil {
			session = s
		}
	}
	var buf strings.Builder
	reply, err := disp.Handle(ctx, msg)
	if err != nil {
		log.Error().Err(err).Str("chat", msg.ChatID).Msg("dispatch error")
		reply = "⚠️ " + err.Error()
	}
	if reply == "" {
		if session != nil {
			_ = session.Final(ctx, "…")
		}
		return
	}
	buf.WriteString(reply)
	if session != nil {
		_ = session.Final(ctx, buf.String())
		return
	}
	if err := c.Send(ctx, messaging.OutgoingMessage{
		ChatID:    msg.ChatID,
		Text:      buf.String(),
		ParseMode: parseMode,
	}); err != nil {
		log.Error().Err(err).Msg("send error")
	}
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
