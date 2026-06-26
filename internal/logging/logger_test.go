package logging_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bestxp/narrative-ai-agent/internal/logging"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNew_DefaultLevel(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	log := logging.New(logging.Config{Writer: &buf})
	log.Info().Msg("hello")

	var rec map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &rec))
	assert.Equal(t, "info", rec["level"])
	assert.Equal(t, "hello", rec["message"])
}

func TestNew_RespectsLevel(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	log := logging.New(logging.Config{Writer: &buf, Level: "error"})
	log.Info().Msg("should be dropped")
	log.Error().Msg("boom")

	out := buf.String()
	assert.NotContains(t, out, "should be dropped")
	assert.Contains(t, out, "boom")
}

func TestNew_UnknownLevelFallsBackToInfo(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	log := logging.New(logging.Config{Writer: &buf, Level: "nonsense"})
	log.Debug().Msg("dropped")
	log.Info().Msg("kept")

	out := buf.String()
	assert.NotContains(t, out, "dropped")
	assert.Contains(t, out, "kept")
}

func TestNew_PrettyWritesToStream(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	log := logging.New(logging.Config{Writer: &buf, Pretty: true, Level: "debug"})
	log.Debug().Msg("pretty")

	out := buf.String()
	assert.True(t, strings.Contains(out, "pretty") || strings.Contains(out, "DEBUG"),
		"expected pretty output, got %q", out)
}

func TestDiscard(t *testing.T) {
	t.Parallel()

	log := logging.Discard()
	// Should not panic; underlying writer is io.Discard so no output.
	assert.NotPanics(t, func() {
		log.Info().Msg("silent")
	})

	lvl := log.GetLevel()
	assert.True(t, lvl == zerolog.Disabled || lvl == zerolog.NoLevel,
		"expected discarded logger, got level %d", lvl)
}
