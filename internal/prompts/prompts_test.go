package prompts

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestList_ContainsExpectedFiles(t *testing.T) {
	list := List()
	assert.Contains(t, list, "narrative.md", "narrative.md must be embedded")
	assert.Contains(t, list, "summary.md", "summary.md must be embedded")
}

func TestBundled_ReturnsContent(t *testing.T) {
	body := Bundled("narrative.md")
	assert.NotEmpty(t, body, "narrative.md must not be empty after embed")
	assert.Contains(t, body, "Game Master", "narrative.md should still mention the role")
}

func TestBundled_PanicsOnMissing(t *testing.T) {
	defer func() {
		r := recover()
		require.NotNil(t, r, "Bundled must panic on missing file")
		assert.Contains(t, r.(string), "missing")
	}()
	_ = Bundled("does-not-exist.md")
}

func TestLoadSystemPrompt_OverrideWins(t *testing.T) {
	dir := t.TempDir()
	override := filepath.Join(dir, "narrative.md")
	require.NoError(t, os.WriteFile(override, []byte("OVERRIDE PROMPT"), 0o600))

	body, err := LoadSystemPrompt(override, "narrative.md")
	require.NoError(t, err)
	assert.Equal(t, "OVERRIDE PROMPT", body)
}

func TestLoadSystemPrompt_FallsBackToBundled(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "no-such-file.md")

	body, err := LoadSystemPrompt(missing, "narrative.md")
	require.NoError(t, err)
	assert.NotEmpty(t, body, "should fall back to embedded narrative.md")
	assert.Contains(t, body, "Game Master")
}

func TestLoadSystemPrompt_OverrideReadError(t *testing.T) {
	// A path that is a directory — ReadFile fails with EISDIR
	// which is NOT a NotExist; we want the error to propagate.
	dir := t.TempDir()
	_, err := LoadSystemPrompt(dir, "narrative.md")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read override")
}

func TestLoadSystemPrompt_TrimsWhitespace(t *testing.T) {
	dir := t.TempDir()
	override := filepath.Join(dir, "narrative.md")
	require.NoError(t, os.WriteFile(override, []byte("  \n  body  \n\n"), 0o600))

	body, err := LoadSystemPrompt(override, "narrative.md")
	require.NoError(t, err)
	assert.Equal(t, "body", body)
}
