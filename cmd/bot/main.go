// Command bot is the narrative-AI agent's runtime binary.
//
// The composition root lives in cmd/bot/app. main() is
// intentionally tiny: parse flags, build the App, run
// the event loop until SIGINT/SIGTERM, then shut down.
//
// Every other concern (config loading, slowlog, LLM
// driver, GM wiring, dispatcher, messaging pool, file
// toolset, per-message handler, auto-save gate, health
// server) is owned by the app package. If you find
// yourself reaching for a new helper here, add it to
// app/ or to the appropriate internal/* package instead.
package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"

	"github.com/bestxp/narrative-ai-agent/cmd/bot/app"
	"github.com/rs/zerolog"
)

func main() {
	cfgPath := flag.String("config", "config.yaml", "path to config.yaml")
	logLevel := flag.String("log-level", "", "override log level (trace/debug/info/warn/error)")
	prettyLog := flag.Bool("log-pretty", false, "human-friendly console writer")
	disableLLM := flag.Bool("no-llm", false, "run without LLM (echo + validation only)")
	flag.Parse()

	a, err := app.NewApp(*cfgPath, *logLevel, *prettyLog, *disableLLM)
	if err != nil {
		// Boot failure: there is no structured logger yet
		// (the config might be the thing that failed to
		// load), so we use a throwaway stderr logger.
		l := zerolog.New(os.Stderr)
		l.Error().Err(err).Msg("boot failed")
		os.Exit(1)
	}
	defer a.Shutdown(context.Background())

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := a.Run(ctx); err != nil {
		a.Log().Error().Err(err).Msg("event loop exited")
		os.Exit(1)
	}
}
