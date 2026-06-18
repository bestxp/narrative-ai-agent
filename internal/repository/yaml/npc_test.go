package yaml

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bestxp/narrative-ai-agent/internal/npcprofile"
)

func (e *testEnv) newNPCProfileRepo() *NPCProfileYaml   { return NewNPCProfileYaml(e.store) }
func (e *testEnv) newNPCRegistryRepo() *NPCRegistryYaml { return NewNPCRegistryYaml(e.store) }

func TestNPCProfileYaml_RoundTrip(t *testing.T) {
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
	env := newTestEnv(t)
	_, err := env.newNPCProfileRepo().Load("naruto", "unknown")
	assert.ErrorIs(t, err, npcprofile.ErrNotFound)
}

func TestNPCProfileYaml_UpdateSection(t *testing.T) {
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

func TestNPCRegistryYaml_AppendEntry(t *testing.T) {
	env := newTestEnv(t)
	require.NoError(t, env.newNPCRegistryRepo().AppendEntry("naruto", "kakashi", "Какаши Хатаке", []string{"Копирующий ниндзя"}))
	require.NoError(t, env.newNPCRegistryRepo().AppendEntry("naruto", "iruka", "Ирука-сенсей", nil))
	out, err := env.newNPCRegistryRepo().Load("naruto")
	require.NoError(t, err)
	assert.Contains(t, out, "Какаши Хатаке")
	assert.Contains(t, out, "kakashi.yaml")
	assert.Contains(t, out, "Копирующий ниндзя")
	assert.Contains(t, out, "Ирука-сенсей")
}

func TestNPCRegistryYaml_AppendEntry_EmptyWorld(t *testing.T) {
	env := newTestEnv(t)
	// First entry on an empty world seeds the table
	// header + the row.
	require.NoError(t, env.newNPCRegistryRepo().AppendEntry("naruto", "kakashi", "Какаши", nil))
	out, _ := env.newNPCRegistryRepo().Load("naruto")
	assert.Contains(t, out, "# NPC: naruto")
	assert.Contains(t, out, "| Имя | Файл | Прозвища |")
	assert.Contains(t, out, "| Какаши |")
}

func TestParseRegistryRow(t *testing.T) {
	displayName, slug, nicknames, ok := ParseRegistryRow("| Какаши | kakashi.yaml | Копирующий ниндзя, Сенсей |")
	require.True(t, ok)
	assert.Equal(t, "Какаши", displayName)
	assert.Equal(t, "kakashi", slug)
	assert.Equal(t, []string{"Копирующий ниндзя", "Сенсей"}, nicknames)

	// Bad row: not a table line.
	_, _, _, ok = ParseRegistryRow("not a table line")
	assert.False(t, ok)
}
