package usecase

import (
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"narrative/internal/adapter/storage"
	"narrative/internal/domain"
)

func seedWorld(t *testing.T, fs *storage.FileStore, world string) {
	t.Helper()
	require.NoError(t, fs.EnsureDir("worlds/"+world+"/characters"))
	require.NoError(t, fs.WriteRawAtomic(storage.InfoFile, domain.BuildInfo("markus", world, nil, nil)))
	require.NoError(t, fs.WriteRawAtomic("worlds/"+world+"/state.md", ""))
	require.NoError(t, fs.WriteRawAtomic("worlds/"+world+"/plan.md", ""))
	require.NoError(t, fs.WriteRawAtomic("worlds/"+world+"/memorise.md", ""))
	require.NoError(t, fs.WriteRawAtomic("worlds/"+world+"/lore.md", ""))
	require.NoError(t, fs.WriteRawAtomic("worlds/"+world+"/canon.md", ""))
}

func TestUpdateState_HeaderAndMoment(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	seedWorld(t, fs, "naruto")
	m := NewMaintenance(fs)
	err := m.UpdateState(StateSnapshot{
		Day: 1, InFlight: true,
		Moment: "Маркус входит в деревню.",
		NPCs:   []string{"Саске", "Какаши"},
	})
	require.NoError(t, err)
	got, _ := fs.ReadRaw("worlds/naruto/state.md")
	assert.True(t, strings.HasPrefix(got, "День 1 (в процессе)."), "header missing in: %q", got)
	assert.Contains(t, got, "Саске, Какаши")
}

func TestUpdateState_HeaderDayFinished(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	seedWorld(t, fs, "naruto")
	m := NewMaintenance(fs)
	require.NoError(t, m.UpdateState(StateSnapshot{Day: 2, InFlight: false, Moment: "x"}))
	got, _ := fs.ReadRaw("worlds/naruto/state.md")
	assert.True(t, strings.HasPrefix(got, "День 2 (завершён)."), "got %q", got)
}

func TestRotatePlan_RejectsBadRange(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	seedWorld(t, fs, "naruto")
	m := NewMaintenance(fs)
	var pe *PlanRangeError
	err := m.RotatePlan("naruto", []string{"a", "b"})
	assert.ErrorAs(t, err, &pe)
	err = m.RotatePlan("naruto", []string{"a", "b", "c", "d", "e", "f"})
	assert.ErrorAs(t, err, &pe)
}

func TestRotatePlan_WritesForwardOnly(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	seedWorld(t, fs, "naruto")
	m := NewMaintenance(fs)
	require.NoError(t, m.RotatePlan("naruto", []string{"event1", "event2", "event3"}))
	got, _ := fs.ReadRaw("worlds/naruto/plan.md")
	assert.NotContains(t, got, "Архив")
	assert.Contains(t, got, "День +1: event1")
}

func TestArchiveDay_SkipsEmpty(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	seedWorld(t, fs, "naruto")
	m := NewMaintenance(fs)
	require.NoError(t, m.ArchiveDay("naruto", 1, "   "))
	got, _ := fs.ReadRaw("worlds/naruto/memorise.md")
	assert.Empty(t, got)
}

func TestArchiveDay_Appends5Digit(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	seedWorld(t, fs, "naruto")
	m := NewMaintenance(fs)
	require.NoError(t, m.ArchiveDay("naruto", 5, "событие"))
	got, _ := fs.ReadRaw("worlds/naruto/memorise.md")
	assert.Contains(t, got, "д00005: событие")
}

func TestArchiveDay_CompressesAfter30(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	seedWorld(t, fs, "naruto")
	m := NewMaintenance(fs)
	for i := 1; i <= 31; i++ {
		require.NoError(t, m.ArchiveDay("naruto", i, "d-"+strconv.Itoa(i)))
	}
	got, _ := fs.ReadRaw("worlds/naruto/memorise.md")
	assert.Equal(t, 1, strings.Count(got, "сводка 30 дней"), "got:\n%s", got)
}

func TestAppendLore_AddsHeader(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	seedWorld(t, fs, "naruto")
	m := NewMaintenance(fs)
	require.NoError(t, m.AppendLore("naruto", "День 3", "новая техника: Расенган-вариант"))
	got, _ := fs.ReadRaw("worlds/naruto/lore.md")
	assert.Contains(t, got, "## День 3")
	assert.Contains(t, got, "Расенган-вариант")
}

func TestAppendMemory_Dedupes(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	require.NoError(t, fs.EnsureDir("characters/markus"))
	require.NoError(t, fs.WriteRawAtomic("characters/markus/memory.md", ""))
	m := NewMaintenance(fs)
	require.NoError(t, m.AppendMemory("markus", "первая встреча с Саске"))
	require.NoError(t, m.AppendMemory("markus", "первая встреча с Саске"))
	got, _ := fs.ReadRaw("characters/markus/memory.md")
	assert.Equal(t, 1, strings.Count(got, "первая встреча с Саске"))
}

func TestCompactNPCs_StripsDatedEvents(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	seedWorld(t, fs, "naruto")
	var b strings.Builder
	b.WriteString("# Какаши\n")
	b.WriteString("- Любит книги\n")
	for i := 0; i < 50; i++ {
		b.WriteString("- 2026-06-01: тренировка #")
		b.WriteString(strconv.Itoa(i))
		b.WriteString("\n")
	}
	require.NoError(t, fs.WriteRawAtomic("worlds/naruto/characters/kakashi.md", b.String()))
	m := NewMaintenance(fs)
	touched, err := m.CompactNPCs("naruto")
	require.NoError(t, err)
	assert.Equal(t, []string{"kakashi"}, touched)
	got, _ := fs.ReadRaw("worlds/naruto/characters/kakashi.md")
	assert.NotContains(t, got, "2026-06-01")
	assert.Contains(t, got, "Любит книги")
}
