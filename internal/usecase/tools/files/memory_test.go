package files

import (
	"context"
	"fmt"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bestxp/narrative-ai-agent/internal/adapter/storage"
	"github.com/bestxp/narrative-ai-agent/internal/chronicle"
	"github.com/bestxp/narrative-ai-agent/internal/npcprofile"
	"github.com/bestxp/narrative-ai-agent/internal/repository/api"
	yamlfs "github.com/bestxp/narrative-ai-agent/internal/storage/fs"
	"github.com/bestxp/narrative-ai-agent/internal/usecase/tools"
)

// newMemoryTestEnv builds a fresh Memory + FileStore on
// the same TempDir. Tests use this to exercise the
// memory methods against a real repository layer.
func newMemoryTestEnv(t *testing.T) (*Memory, *storage.FileStore) {
	t.Helper()
	fs, err := storage.NewFileStore(t.TempDir())
	require.NoError(t, err)
	yamlStore, err := yamlfs.New(fs.Root())
	require.NoError(t, err)
	repos := api.NewYamlRepositories(yamlStore)
	return newMemory(zerolog.Nop(), nil, nil, nil, nil, repos), fs
}

// writeLongNPC writes a profile with 50 personal_memory
// entries (over the threshold of 40).
func writeLongNPC(t *testing.T, fs *storage.FileStore, world, slug, display string) {
	t.Helper()
	p := npcprofile.Profile{DisplayName: display, FileSlug: slug}
	for i := range 50 {
		p.PersonalMemory = append(p.PersonalMemory, "факт "+itoa(i))
	}
	body, err := p.Save()
	require.NoError(t, err)
	require.NoError(t, fs.WriteRawAtomic("worlds/"+world+"/characters/"+slug+".yaml", body))
}

// itoa is a tiny stdlib-free int-to-string helper.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// --- AppendLore ---

func TestAppendLore_AppendsToExisting(t *testing.T) {
	t.Parallel()
	m, _ := newMemoryTestEnv(t)
	require.NoError(t, m.repos.Lore.AppendEntry("naruto", "Канон", "Хокаге умер в бою"))
	require.NoError(t, m.AppendLore("naruto", "Лор", "Какаши носит маску"))
	out, _ := m.repos.Lore.Load("naruto")
	assert.Contains(t, out, "## Канон")
	assert.Contains(t, out, "## Лор")
	assert.Contains(t, out, "- Какаши носит маску")
}

func TestAppendLore_EmptyHeaderRejected(t *testing.T) {
	t.Parallel()
	m, _ := newMemoryTestEnv(t)
	assert.Error(t, m.AppendLore("naruto", "", "x"))
}

// --- MaintainNPCs ---

func TestMaintainNPCs_NilSummarizerWarnsAndSkips(t *testing.T) {
	t.Parallel()
	m, fs := newMemoryTestEnv(t)
	writeLongNPC(t, fs, "naruto", "kakashi", "Какаши")
	touched, err := m.MaintainNPCs("naruto")
	require.NoError(t, err)
	assert.Empty(t, touched)
}

// --- MaintainLore ---

func TestMaintainLore_UnderThresholdSkips(t *testing.T) {
	t.Parallel()
	m, _ := newMemoryTestEnv(t)
	require.NoError(t, m.repos.Lore.AppendEntry("naruto", "H", "b"))
	ok, err := m.MaintainLore(context.Background(), "naruto")
	require.NoError(t, err)
	assert.False(t, ok)
}

// --- ChronicleCompressWindow ---

func TestChronicleCompressWindow_Basic30Days(t *testing.T) {
	t.Parallel()
	m, _ := newMemoryTestEnv(t)
	for i := 1; i <= 30; i++ {
		require.NoError(t, appendDay(t, m, i, "день "+itoa(i)))
	}
	stub := &stubChronicleSummarizer{returnedMemory: "выжимка 30 дней"}
	m.chronicleSummarizer = stub
	ok, err := m.ChronicleCompressWindow(context.Background(), "naruto", 1, 30)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, 1, stub.calls)
	c, err := m.repos.Chronicle.Load("naruto")
	require.NoError(t, err)
	require.Len(t, c.Periods, 1)
	assert.Equal(t, 1, c.Periods[0].From)
	assert.Equal(t, 30, c.Periods[0].To)
	assert.Equal(t, "выжимка 30 дней", c.Periods[0].Memory)
	assert.Empty(t, c.Days)
}

func TestChronicleCompressWindow_NoSummarizerSkips(t *testing.T) {
	t.Parallel()
	m, _ := newMemoryTestEnv(t)
	for i := 1; i <= 30; i++ {
		require.NoError(t, appendDay(t, m, i, "день "+itoa(i)))
	}
	ok, err := m.ChronicleCompressWindow(context.Background(), "naruto", 1, 30)
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestChronicleCompressWindow_BadOutputSkips(t *testing.T) {
	t.Parallel()
	m, _ := newMemoryTestEnv(t)
	for i := 1; i <= 30; i++ {
		require.NoError(t, appendDay(t, m, i, "день "+itoa(i)))
	}
	stub := &stubChronicleSummarizer{skipOutput: true}
	m.chronicleSummarizer = stub
	before, _ := m.repos.Chronicle.Load("naruto")
	ok, err := m.ChronicleCompressWindow(context.Background(), "naruto", 1, 30)
	require.NoError(t, err)
	assert.False(t, ok)
	after, _ := m.repos.Chronicle.Load("naruto")
	assert.Len(t, after.Periods, len(before.Periods))
	assert.Len(t, after.Days, len(before.Days))
}

func TestChronicleCompressWindow_TooThinSkips(t *testing.T) {
	t.Parallel()
	m, _ := newMemoryTestEnv(t)
	for _, d := range []int{1} {
		require.NoError(t, appendDay(t, m, d, "факт"))
	}
	stub := &stubChronicleSummarizer{returnedMemory: "should not be called"}
	m.chronicleSummarizer = stub
	ok, err := m.ChronicleCompressWindow(context.Background(), "naruto", 1, 30)
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Equal(t, 0, stub.calls)
}

func TestChronicleCompressWindow_KeepsEarlierSummaries(t *testing.T) {
	t.Parallel()
	m, _ := newMemoryTestEnv(t)
	c := chronicle.Chronicle{
		Periods: []chronicle.Period{
			{From: 1, To: 30, Memory: "старая"},
		},
		Days: map[int]string{},
	}
	for i := 31; i <= 60; i++ {
		c.AppendDay(i, "день "+itoa(i))
	}
	require.NoError(t, m.repos.Chronicle.Save("naruto", c))
	stub := &stubChronicleSummarizer{returnedMemory: "новая"}
	m.chronicleSummarizer = stub
	ok, err := m.ChronicleCompressWindow(context.Background(), "naruto", 1, 60)
	require.NoError(t, err)
	assert.True(t, ok)
	out, _ := m.repos.Chronicle.Load("naruto")
	require.Len(t, out.Periods, 2)
	assert.Equal(t, "старая", out.Periods[0].Memory)
	assert.Equal(t, "новая", out.Periods[1].Memory)
}

// --- helpers ---

// appendDay writes a raw day entry to the chronicle
// using the repository API. Bypasses ArchiveChronicleDay
// so the test can pre-stage a 30-day window. The world
// is hard-coded to "naruto" because every test in this
// file operates on that fixture.
func appendDay(t *testing.T, m *Memory, day int, text string) error {
	t.Helper()
	world := "naruto"
	c, err := m.repos.Chronicle.Load(world)
	if err != nil {
		return fmt.Errorf("appendDay: Chronicle.Load failed: %w", err)
	}
	if !c.AppendDay(day, text) {
		t.Fatalf("appendDay: day %d already present", day)
	}
	return m.repos.Chronicle.Save(world, c)
}

// stubChronicleSummarizer is a deterministic test double
// for tools.ChronicleSummarizer.
type stubChronicleSummarizer struct {
	calls          int
	firstFrom      int
	firstTo        int
	lastFrom       int
	lastTo         int
	returnedMemory string
	skipOutput     bool
	err            error
}

func (s *stubChronicleSummarizer) SummarizeChronicle(_ context.Context, _ string, startDay, endDay int, _ string) ([]byte, error) {
	s.calls++
	if s.calls == 1 {
		s.firstFrom = startDay
		s.firstTo = endDay
	}
	s.lastFrom = startDay
	s.lastTo = endDay
	if s.err != nil {
		return nil, s.err
	}
	if s.skipOutput {
		return nil, nil
	}
	return []byte(s.returnedMemory), nil
}

// Compile-time guarantee.
var _ tools.ChronicleSummarizer = (*stubChronicleSummarizer)(nil)
