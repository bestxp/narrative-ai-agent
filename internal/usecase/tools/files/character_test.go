package files

import (
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bestxp/narrative-ai-agent/internal/adapter/storage"
	"github.com/bestxp/narrative-ai-agent/internal/charprofile"
	"github.com/bestxp/narrative-ai-agent/internal/repository/api"
	"github.com/bestxp/narrative-ai-agent/internal/slowlog"
	yamlfs "github.com/bestxp/narrative-ai-agent/internal/storage/fs"
)

func newTestCharacter(t *testing.T) (*Character, *storage.FileStore) {
	t.Helper()
	fs, _ := storage.NewFileStore(t.TempDir())
	yamlStore, _ := yamlfs.New(fs.Root())
	repos := api.NewYamlRepositories(yamlStore)
	c := newCharacter(repos, zerolog.Nop(), slowlog.Discard())
	return c, fs
}

// --- Soul ---

func TestSoulYaml_AppendSection(t *testing.T) {
	c, _ := newTestCharacter(t)
	ok, err := c.AppendSoul("markus", "Предпочтения", "Ирука-сенсей")
	require.NoError(t, err)
	assert.True(t, ok)
	ok, _ = c.AppendSoul("markus", "Предпочтения", "Ирука-сенсей")
	assert.False(t, ok)
}

// --- Skill ---

func TestSkillYaml_AppendSection(t *testing.T) {
	c, _ := newTestCharacter(t)
	ok, err := c.AppendSkill("markus", "Ранг", "генин")
	require.NoError(t, err)
	assert.True(t, ok)
}

// --- Memory ---

func TestMemoryYaml_AppendSection(t *testing.T) {
	c, _ := newTestCharacter(t)
	ok, err := c.AppendMemorySection("markus", "Яркие моменты", "день 1: встреча с Какаши")
	require.NoError(t, err)
	assert.True(t, ok)
}

// --- Inventory ---

func TestInventoryYaml_AppendItem(t *testing.T) {
	c, _ := newTestCharacter(t)
	ok, err := c.AppendInventoryItem("markus", charprofile.Item{
		Name: "Кунай", Description: "стандартный", Equip: true,
	})
	require.NoError(t, err)
	assert.True(t, ok)
}

// --- Read ---
