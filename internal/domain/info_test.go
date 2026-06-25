package domain

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const sampleInfo = `active_character: markus
active_world: naruto
characters:
  - markus
worlds:
  - naruto
  - bleach
`

func TestParseInfo_FromYAML(t *testing.T) {
	t.Parallel()
	info, err := ParseInfo(sampleInfo)
	require.NoError(t, err)
	assert.Equal(t, "markus", info.ActiveCharacter)
	assert.Equal(t, "naruto", info.ActiveWorld)
	assert.Equal(t, []string{"markus"}, info.Characters)
	assert.Equal(t, []string{"naruto", "bleach"}, info.Worlds)
}

func TestParseInfo_Pointers(t *testing.T) {
	t.Parallel()
	info, err := ParseInfo(sampleInfo)
	require.NoError(t, err)
	assert.Equal(t, "characters/markus", info.ActiveCharacterPointer())
	assert.Equal(t, "worlds/naruto", info.ActiveWorldPointer())
}

func TestParseInfo_EmptyPlaceholdersAllowed(t *testing.T) {
	t.Parallel()
	// Freshly bootstrapped registry is a valid Info with zero values —
	// SessionStart will fill it in via /launch.
	info, err := ParseInfo(BuildInfo("", "", nil, nil))
	require.NoError(t, err)
	assert.Empty(t, info.ActiveCharacter)
	assert.Empty(t, info.ActiveWorld)
	assert.Empty(t, info.Characters)
	assert.Empty(t, info.Worlds)
}

func TestParseInfo_EmptyBodyErrors(t *testing.T) {
	t.Parallel()
	_, err := ParseInfo("")
	assert.Error(t, err)
}

func TestParseInfo_BadYAMLErrors(t *testing.T) {
	t.Parallel()
	_, err := ParseInfo("active_character: : :")
	assert.Error(t, err)
}

func TestBuildInfo_RoundTrip(t *testing.T) {
	t.Parallel()
	out := BuildInfo("markus", "naruto", []string{"alice"}, []string{"bleach"})
	info, err := ParseInfo(out)
	require.NoError(t, err)
	assert.Equal(t, "markus", info.ActiveCharacter)
	assert.Equal(t, "naruto", info.ActiveWorld)
	assert.ElementsMatch(t, []string{"markus", "alice"}, info.Characters)
	assert.ElementsMatch(t, []string{"naruto", "bleach"}, info.Worlds)
}

func TestBuildInfo_Dedupes(t *testing.T) {
	t.Parallel()
	out := BuildInfo("markus", "naruto", []string{"markus", "alice"}, []string{"naruto", "bleach"})
	info, err := ParseInfo(out)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"markus", "alice"}, info.Characters)
	assert.ElementsMatch(t, []string{"naruto", "bleach"}, info.Worlds)
}

func TestBuildInfo_EmptyProducesValidYAML(t *testing.T) {
	t.Parallel()
	out := BuildInfo("", "", nil, nil)
	info, err := ParseInfo(out)
	require.NoError(t, err)
	assert.Empty(t, info.ActiveCharacter)
	assert.Empty(t, info.ActiveWorld)
	assert.Empty(t, info.Characters)
	assert.Empty(t, info.Worlds)
}

func TestRenderSample(t *testing.T) {
	t.Parallel()
	// Sanity-check the file shape the bot will write to disk.
	out := BuildInfo("markus", "naruto", []string{"alice"}, []string{"bleach"})
	t.Logf("\n%s", out)
	assert.Contains(t, out, "active_character: markus")
	assert.Contains(t, out, "active_world: naruto")
	assert.Contains(t, out, "- markus")
	assert.Contains(t, out, "- alice")
	assert.Contains(t, out, "- naruto")
	assert.Contains(t, out, "- bleach")
	assert.NotContains(t, out, "---", "info.yaml must be pure YAML, no frontmatter fences")
}
