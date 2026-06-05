package domain

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseInfo(t *testing.T) {
	body := `# info
- [АКТИВЕН] characters/markus
- [АКТИВЕН] worlds/naruto
- [НЕАКТИВЕН] worlds/bleach
`
	c, w, err := ParseInfo(body)
	require.NoError(t, err)
	assert.True(t, c.Active)
	assert.Equal(t, "characters/markus", c.Pointer)
	assert.True(t, w.Active)
	assert.Equal(t, "worlds/naruto", w.Pointer)
}

func TestParseInfo_Empty(t *testing.T) {
	_, _, err := ParseInfo("")
	assert.Error(t, err)
}

func TestBuildInfo_ContainsAnchors(t *testing.T) {
	out := BuildInfo("m", "w", nil, nil)
	assert.Contains(t, out, "[АКТИВЕН] characters/m")
	assert.Contains(t, out, "[АКТИВЕН] worlds/w")
	assert.Contains(t, out, "## Правила")
}
