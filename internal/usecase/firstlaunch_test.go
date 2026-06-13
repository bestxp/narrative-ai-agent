package usecase

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bestxp/narrative-ai-agent/internal/adapter/storage"
)

func TestFirstLaunch_CreatesSkeleton(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	fl := NewFirstLaunch(fs)
	err := fl.Launch(
		CharacterSpec{DisplayName: "Маркус", Dir: "Маркус", TrueNature: "человек", Philosophy: "воля"},
		WorldSpec{DisplayName: "Наруто", Dir: "naruto", IsKnown: true, Canon: "деревня скрытого листа"},
	)
	require.NoError(t, err)
	for _, rel := range []string{
		storage.InfoFile,
		"characters/markus/SOUL.yaml",
		"characters/markus/skill.yaml",
		"characters/markus/memory.yaml",
		"characters/markus/inventory.yaml",
		"worlds/naruto/canon.md",
		"worlds/naruto/state.md",
		"worlds/naruto/lore.md",
		"worlds/naruto/plan.md",
		"worlds/naruto/memorise.md",
		"worlds/naruto/characters.md",
		"worlds/naruto/characters",
	} {
		assert.True(t, fs.Exists(rel), "missing: %s", rel)
	}
}

func TestFirstLaunch_Idempotent(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	fl := NewFirstLaunch(fs)
	require.NoError(t, fl.Launch(CharacterSpec{Dir: "a", DisplayName: "A"}, WorldSpec{Dir: "b", DisplayName: "B", Canon: "x"}))
	err := fl.Launch(CharacterSpec{Dir: "c", DisplayName: "C"}, WorldSpec{Dir: "d", DisplayName: "D", Canon: "x"})
	assert.ErrorIs(t, err, ErrAlreadyLaunched)
}

func TestFirstLaunch_TransliteratesCyrillic(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	fl := NewFirstLaunch(fs)
	require.NoError(t, fl.Launch(CharacterSpec{Dir: "Маркус", DisplayName: "Маркус"}, WorldSpec{Dir: "ВанПис", DisplayName: "ВанПис", Canon: "x"}))
	assert.True(t, fs.Exists("characters/markus"))
	assert.True(t, fs.Exists("worlds/vanpis"))
}
