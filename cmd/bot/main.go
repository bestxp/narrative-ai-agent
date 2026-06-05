package main

import (
	"flag"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/rs/zerolog"

	"narrative/internal/adapter/gitops"
	"narrative/internal/adapter/storage"
	"narrative/internal/adapter/telegrambot"
	"narrative/internal/config"
	"narrative/internal/logging"
)

func main() {
	cfgPath := flag.String("config", "config.yaml", "path to config.yaml")
	logLevel := flag.String("log-level", "", "override log level (trace/debug/info/warn/error)")
	prettyLog := flag.Bool("log-pretty", false, "human-friendly console writer")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		// Logger not yet built — fall back to stderr so init failures
		// are still visible to operators.
		l := zerolog.New(os.Stderr)
		l.Error().Err(err).Msg("config load failed")
		os.Exit(1)
	}
	_ = cfg // silence if unused later

	level := *logLevel
	if level == "" {
		level = "info"
	}
	log := logging.New(logging.Config{Level: level, Pretty: *prettyLog})

	log.Info().Str("config", *cfgPath).Msg("starting lazy-universe bot")

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
	git := gitops.NewWithLogger(cfg.Paths.GitWorkdir, cfg.Git.Remote, cfg.Git.Branch, cfg.Git.CommitAuthor, cfg.Git.CommitEmail, log)

	dispatcher := telegrambot.NewDispatcherWithLogger(cfg, fs, git, log)
	bot, err := telegrambot.NewWithLogger(cfg.Telegram.Token, cfg, dispatcher, log)
	if err != nil {
		log.Fatal().Err(err).Msg("telegram init")
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		log.Info().Str("signal", sig.String()).Msg("shutdown")
		os.Exit(0)
	}()

	if err := bot.Run(); err != nil {
		log.Fatal().Err(err).Msg("bot run")
	}
}
