// Package logging provides a single configured zerolog.Logger shared
// across the application. Adapters and usecases receive the logger via
// constructor injection — never via a global — so tests can swap in a
// buffer-backed instance.
package logging

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

type Config struct {
	Level  string
	Pretty bool
	Writer io.Writer
}

func New(cfg Config) zerolog.Logger {
	return NewWithSlowlog(cfg, nil)
}

// NewWithSlowlog returns a zerolog.Logger that writes JSON
// lines to both the primary writer AND the slowlog writer.
// When cfg.Pretty is true, the console gets a human-friendly
// ConsoleWriter while the slowlog gets the raw JSON — this
// is achieved by tee-ing the raw JSON to slowlog first, then
// feeding it into the ConsoleWriter for the console output.
// When cfg.Pretty is false, a simple MultiWriter feeds both
// stderr and slowlog the same bytes.
func NewWithSlowlog(cfg Config, slow *SlowlogWriter) zerolog.Logger {
	w := cfg.Writer
	if w == nil {
		w = os.Stderr
	}
	level, err := zerolog.ParseLevel(strings.ToLower(cfg.Level))
	if err != nil || level == zerolog.NoLevel {
		level = zerolog.InfoLevel
	}
	zerolog.TimeFieldFormat = time.RFC3339

	if slow != nil {
		if cfg.Pretty {
			// ConsoleWriter consumes JSON and renders
			// tabular text. We need the raw JSON to also
			// reach slowlog. Solution: write the raw JSON
			// to slowlog first, THEN pipe through
			// ConsoleWriter to stderr.
			console := zerolog.ConsoleWriter{Out: w, TimeFormat: time.RFC3339}
			return zerolog.New(io.MultiWriter(slow, console)).Level(level).With().Timestamp().Logger()
		}
		// Plain JSON mode: both stderr and slowlog get the
		// same bytes.
		return zerolog.New(io.MultiWriter(w, slow)).Level(level).With().Timestamp().Logger()
	}

	if cfg.Pretty {
		w = zerolog.ConsoleWriter{Out: w, TimeFormat: time.RFC3339}
	}

	return zerolog.New(w).Level(level).With().Timestamp().Logger()
}

func Discard() zerolog.Logger {
	return zerolog.New(io.Discard).Level(zerolog.NoLevel)
}

// SlowlogWriter wraps an io.Writer (typically the slowlog
// file handle) so that concurrent zerolog writes and slowlog
// Write() calls from the GM layer do not interleave within a
// line. Each Write call appends the bytes as-is — zerolog
// already serialises each event as a single JSON line
// terminated by '\n', so the output is valid JSON-lines.
type SlowlogWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func NewSlowlogWriter(w io.Writer) *SlowlogWriter {
	return &SlowlogWriter{w: w}
}

func (s *SlowlogWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, err := s.w.Write(p)
	if err != nil {
		return n, fmt.Errorf("slowlog_writer: %w", err)
	}

	return n, nil
}
