package yaml_test

import (
	"testing"

	"github.com/bestxp/narrative-ai-agent/internal/npcprofile"
	"github.com/bestxp/narrative-ai-agent/internal/repository/yaml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func (e *testEnv) newNPCProfileRepo() *yaml.NPCProfileYaml { return yaml.NewNPCProfileYaml(e.store) }

func TestNPCProfileYaml_RoundTrip(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	in := npcprofile.Profile{
		DisplayName: "Какаши Хатаке",
		FileSlug:    "kakashi",
		Temperament: "спокойный",
	}
	require.NoError(t, env.newNPCProfileRepo().Save("naruto", "kakashi", in))

	out, err := env.newNPCProfileRepo().Load("naruto", "kakashi")
	require.NoError(t, err)
	assert.Equal(t, in.DisplayName, out.DisplayName)
	assert.Equal(t, in.FileSlug, out.FileSlug)
}

func TestNPCProfileYaml_LoadMissing(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	_, err := env.newNPCProfileRepo().Load("naruto", "unknown")
	assert.ErrorIs(t, err, npcprofile.ErrNotFound)
}

func TestNPCProfileYaml_UpdateSection(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	require.NoError(t, env.newNPCProfileRepo().Save("naruto", "kakashi", npcprofile.Profile{
		DisplayName: "Какаши Хатаке",
		FileSlug:    "kakashi",
	}))
	ok, err := env.newNPCProfileRepo().UpdateSection("naruto", "kakashi", "abilities", "Шаринган")
	require.NoError(t, err)
	assert.True(t, ok)

	out, _ := env.newNPCProfileRepo().Load("naruto", "kakashi")
	assert.Equal(t, []string{"Шаринган"}, out.Abilities)
}
