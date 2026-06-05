package usecase

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"narrative/internal/adapter/storage"
	"narrative/internal/domain"
)

func TestWorldTransition_Leave_CreatesNew(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	seedWorld(t, fs, "naruto")
	// bleach is not in the worlds list yet — switchActive must add it.
	require.NoError(t, fs.WriteRawAtomic(storage.InfoFile, domain.BuildInfo("markus", "naruto", nil, nil)))
	wt := NewWorldTransition(fs)
	res, err := wt.Leave("naruto", "bleach", "1 час", "markus")
	require.NoError(t, err)
	assert.True(t, res.NewWorldInit)
	for _, rel := range []string{
		"worlds/bleach/state.md",
		"worlds/bleach/plan.md",
		"worlds/bleach/canon.md",
	} {
		assert.True(t, fs.Exists(rel), "missing %s", rel)
	}
	info, _ := fs.ReadRaw(storage.InfoFile)
	parsed, err := domain.ParseInfo(info)
	require.NoError(t, err)
	assert.Equal(t, "bleach", parsed.ActiveWorld)
	assert.Contains(t, parsed.Worlds, "bleach")
	assert.Contains(t, parsed.Worlds, "naruto", "naruto stays in the list for future return")
}

func TestWorldTransition_Leave_CompressesState(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	seedWorld(t, fs, "naruto")
	require.NoError(t, fs.WriteRawAtomic(storage.InfoFile, domain.BuildInfo("markus", "naruto", nil, nil)))
	require.NoError(t, fs.WriteRawAtomic("worlds/naruto/state.md", "День 7 (в процессе).\nДлинный текст сцены...\n"))
	wt := NewWorldTransition(fs)
	_, err := wt.Leave("naruto", "bleach", "", "")
	require.NoError(t, err)
	got, _ := fs.ReadRaw("worlds/naruto/state.md")
	assert.Contains(t, got, "Уход в мир bleach")
	assert.NotContains(t, got, "Длинный текст сцены")
}

func TestWorldTransition_Leave_AppendsMemory(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	seedWorld(t, fs, "naruto")
	require.NoError(t, fs.WriteRawAtomic(storage.InfoFile, domain.BuildInfo("markus", "naruto", nil, nil)))
	require.NoError(t, fs.EnsureDir("characters/markus"))
	require.NoError(t, fs.WriteRawAtomic("characters/markus/memory.md", ""))
	wt := NewWorldTransition(fs)
	_, err := wt.Leave("naruto", "bleach", "5 минут", "markus")
	require.NoError(t, err)
	mem, _ := fs.ReadRaw("characters/markus/memory.md")
	assert.Contains(t, mem, "Переход в мир bleach")
}

func TestWorldTransition_Return_AdvancesDay(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	seedWorld(t, fs, "naruto")
	require.NoError(t, fs.WriteRawAtomic(storage.InfoFile, domain.BuildInfo("markus", "bleach", nil, []string{"naruto"})))
	require.NoError(t, fs.WriteRawAtomic("worlds/naruto/state.md", "День 3 (завершён).\nУход.\n"))
	wt := NewWorldTransition(fs)
	note, err := wt.ReturnWorld("naruto", "5")
	require.NoError(t, err)
	assert.Contains(t, note, "Прошло 5 дн.")
	got, _ := fs.ReadRaw("worlds/naruto/state.md")
	assert.True(t, strings.HasPrefix(got, "День 8 (в процессе)."), "got %q", got)
}

func TestWorldTransition_Return_RejectsBadDays(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	seedWorld(t, fs, "naruto")
	wt := NewWorldTransition(fs)
	_, err := wt.ReturnWorld("naruto", "abc")
	assert.Error(t, err)
	_, err = wt.ReturnWorld("naruto", "-1")
	assert.Error(t, err)
}

func TestWorldTransition_Logs(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	seedWorld(t, fs, "naruto")
	log, buf := newBufLogger()
	wt := NewWorldTransitionWithLogger(fs, log)
	require.NoError(t, fs.WriteRawAtomic(storage.InfoFile, domain.BuildInfo("markus", "naruto", nil, nil)))
	_, err := wt.Leave("naruto", "bleach", "", "")
	require.NoError(t, err)
	assert.Contains(t, buf.String(), "world_leave")
}
