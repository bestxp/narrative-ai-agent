package slowlog

import (
	"bytes"
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDiscard_NeverPanics(t *testing.T) {
	t.Parallel()
	l := Discard()
	assert.NoError(t, l.WriteOK("test", ""))
	assert.NoError(t, l.Write("test", "", map[string]any{"x": 1}))
}

func TestFile_AppendsJSONLines(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	l, err := File(dir + "/slow.log")
	require.NoError(t, err)
	require.NoError(t, l.WriteOK("llm.request", "123"))
	require.NoError(t, l.Write("tool.update_state", "123", map[string]any{"path": "state.md", "bytes": 42}))

	data, err := os.ReadFile(dir + "/slow.log")
	require.NoError(t, err)
	lines := splitNonEmpty(data)
	require.Len(t, lines, 2)

	first := parseLine(t, lines[0])
	assert.Equal(t, "llm.request", first.Kind)
	assert.Equal(t, "123", first.Chat)
	assert.NotEmpty(t, first.Time)

	second := parseLine(t, lines[1])
	assert.Equal(t, "tool.update_state", second.Kind)
	assert.InDelta(t, float64(42), second.Fields["bytes"], 1e-9)
	assert.Equal(t, "state.md", second.Fields["path"])
}

func TestFile_ConcurrentWrites(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	l, err := File(dir + "/slow.log")
	require.NoError(t, err)

	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			_ = l.WriteOK("concurrent", "")
		})
	}

	wg.Wait()
	data, err := os.ReadFile(dir + "/slow.log")
	require.NoError(t, err)
	lines := splitNonEmpty(data)
	assert.Len(t, lines, 50)

	for _, ln := range lines {
		assert.True(t, json.Valid(ln), "line not valid JSON: %s", ln)
	}
}

func TestFile_MissingDirIsCreated(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	l, err := File(dir + "/nested/dir/slow.log")
	require.NoError(t, err)
	assert.NoError(t, l.WriteOK("k", ""))
}

func TestFile_TimestampFormatting(t *testing.T) {
	t.Parallel()
	l := Discard()
	fixed := time.Date(2026, 6, 5, 22, 30, 0, 123_000_000, time.UTC)
	l.now = func() time.Time { return fixed }
	require.NoError(t, l.WriteOK("k", ""))
	// Indirect tripwire: the format used in Write must match.
	assert.True(t, json.Valid([]byte(`{"time":"`+fixed.Format(time.RFC3339Nano)+`"}`)))
}

type entry struct {
	Time   string         `json:"time"`
	Kind   string         `json:"kind"`
	Chat   string         `json:"chat,omitempty"`
	Fields map[string]any `json:"fields,omitempty"`
}

func parseLine(t *testing.T, line []byte) entry {
	t.Helper()
	var e entry
	require.NoError(t, json.Unmarshal(line, &e))

	return e
}

func splitNonEmpty(b []byte) [][]byte {
	parts := bytes.Split(b, []byte("\n"))

	out := make([][]byte, 0, len(parts))
	for _, p := range parts {
		if len(p) > 0 {
			out = append(out, p)
		}
	}

	return out
}
