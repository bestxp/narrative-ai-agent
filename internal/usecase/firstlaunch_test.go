package usecase

import (
	"testing"

	"github.com/bestxp/narrative-ai-agent/internal/adapter/storage"
	"github.com/bestxp/narrative-ai-agent/internal/repository/api"
	yamlfs "github.com/bestxp/narrative-ai-agent/internal/storage/fs"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFirstLaunch_CreatesSkeleton(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	ys, _ := yamlfs.New(fs.Root())
	repos := api.NewYamlRepositories(ys)
	fl := NewFirstLaunchWithLogger(fs, repos.WorldState, zerolog.Nop())
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
		"worlds/naruto/state.yaml",
		"worlds/naruto/lore.md",
		"worlds/naruto/plan.md",
		"worlds/naruto/chronicle.yaml",
		"worlds/naruto/characters",
	} {
		assert.True(t, fs.Exists(rel), "missing: %s", rel)
	}
	// planning/0001: state.yaml carries the stage
	// baseline from the first turn — the file must
	// already contain the `stage:` block, not be an
	// empty placeholder.
	body, _ := fs.ReadRaw("worlds/naruto/state.yaml")
	assert.Contains(t, body, "stage:")
	assert.Contains(t, body, "current: \"\"")
	assert.Contains(t, body, "timeline-index: 0")
	assert.Contains(t, body, `next: ""`)
}

func TestFirstLaunch_Idempotent(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	ys, _ := yamlfs.New(fs.Root())
	repos := api.NewYamlRepositories(ys)
	fl := NewFirstLaunchWithLogger(fs, repos.WorldState, zerolog.Nop())
	require.NoError(t, fl.Launch(CharacterSpec{Dir: "a", DisplayName: "A"}, WorldSpec{Dir: "b", DisplayName: "B", Canon: "x"}))
	err := fl.Launch(CharacterSpec{Dir: "c", DisplayName: "C"}, WorldSpec{Dir: "d", DisplayName: "D", Canon: "x"})
	assert.ErrorIs(t, err, ErrAlreadyLaunched)
}

func TestFirstLaunch_TransliteratesCyrillic(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	ys, _ := yamlfs.New(fs.Root())
	repos := api.NewYamlRepositories(ys)
	fl := NewFirstLaunchWithLogger(fs, repos.WorldState, zerolog.Nop())
	require.NoError(t, fl.Launch(CharacterSpec{Dir: "Маркус", DisplayName: "Маркус"}, WorldSpec{Dir: "ВанПис", DisplayName: "ВанПис", Canon: "x"}))
	assert.True(t, fs.Exists("characters/markus"))
	assert.True(t, fs.Exists("worlds/vanpis"))
}
