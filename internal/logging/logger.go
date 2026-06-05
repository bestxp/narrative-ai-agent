// Package logging provides a single configured zerolog.Logger shared
// across the application. Adapters and usecases receive the logger via
// constructor injection — never via a global — so tests can swap in a
// buffer-backed instance.
package logging

import (
	"io"
	"os"
	"strings"
	"time"

	"github.com/rs/zerolog"
)

// Config controls how the global logger is built.
type Config struct {
	Level  string // "trace", "debug", "info", "warn", "error"
	Pretty bool   // human-friendly console writer (dev)
	Writer io.Writer
}

// New returns a configured logger. If cfg is zero-valued, a sensible
// default is used (info level, stderr, plain).
func New(cfg Config) zerolog.Logger {
	w := cfg.Writer
	if w == nil {
		w = os.Stderr
	}
	level, err := zerolog.ParseLevel(strings.ToLower(cfg.Level))
	if err != nil || level == zerolog.NoLevel {
		level = zerolog.InfoLevel
	}
	zerolog.TimeFieldFormat = time.RFC3339
	if cfg.Pretty {
		w = zerolog.ConsoleWriter{Out: w, TimeFormat: time.RFC3339}
	}
	return zerolog.New(w).Level(level).With().Timestamp().Logger()
}

// Discard returns a logger that swallows every record. Useful in tests
// for components that log but should not pollute test output. The
// level is set to NoLevel so callers may still call any method
// without panics.
func Discard() zerolog.Logger {
	return zerolog.New(io.Discard).Level(zerolog.NoLevel)
}
