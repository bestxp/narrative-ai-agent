// Package slowlog is a per-request audit log. Every entry is a
// timestamped JSON object on a single line; entries are written
// under an exclusive lock so concurrent goroutines (the LLM
// stream callback, the file-store writer, the dispatcher) do not
// interleave within a line.
//
// Two flavours exist:
//
//   - File wraps a real *os.File. The bot process opens it in
//     append mode and keeps it open for the lifetime of the run.
//   - Discard is a no-op. Use it in tests and when the operator
//     has explicitly turned the slowlog off via config.
//
// The package does not try to be Postgres slowlog: there is no
// sampling, no per-statement threshold, no rotation. Operators
// rotate the file by hand (or via logrotate) and the bot is
// expected to run with a small per-character multiverse.
package slowlog

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Logger is the public surface. Methods are safe for concurrent
// use.
type Logger struct {
	w   io.Writer
	mu  *sync.Mutex
	now func() time.Time
}

// File opens (or creates, append-mode) path and returns a Logger
// that writes JSON lines to it. The parent directory is created
// if missing — operators usually point File at ./slow.log or
// ~/.cache/lazy-universe/slow.log.
func File(path string) (*Logger, error) {
	if path == "" {
		return Discard(), nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("slowlog: mkdir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("slowlog: open %s: %w", path, err)
	}
	return &Logger{w: f, mu: &sync.Mutex{}, now: time.Now}, nil
}

// Discard returns a Logger that drops every entry. Cheap.
func Discard() *Logger {
	return &Logger{w: io.Discard, mu: &sync.Mutex{}, now: time.Now}
}

// Entry is the wire format of a slowlog line. The caller picks a
// "kind" tag (e.g. "llm.request", "tool.update_state") and adds
// any structured fields via Fields.
type Entry struct {
	Time   string         `json:"time"`
	Kind   string         `json:"kind"`
	Chat   string         `json:"chat,omitempty"`
	Fields map[string]any `json:"fields,omitempty"`
}

// Write serialises a single entry. Errors writing to disk are
// surfaced — slowlog is a debugging tool, silent failure would
// defeat its purpose.
func (l *Logger) Write(kind, chat string, fields map[string]any) error {
	entry := Entry{
		Time:   l.now().UTC().Format(time.RFC3339Nano),
		Kind:   kind,
		Chat:   chat,
		Fields: fields,
	}
	buf, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("slowlog: marshal: %w", err)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, err := l.w.Write(append(buf, '\n')); err != nil {
		return fmt.Errorf("slowlog: write: %w", err)
	}
	return nil
}

// WriteOK is the no-fields shortcut. Equivalent to
// Write(kind, chat, nil).
func (l *Logger) WriteOK(kind, chat string) error {
	return l.Write(kind, chat, nil)
}
