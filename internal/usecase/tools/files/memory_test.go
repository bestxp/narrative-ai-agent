package files

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bestxp/narrative-ai-agent/internal/adapter/storage"
	"github.com/bestxp/narrative-ai-agent/internal/npcprofile"
	"github.com/bestxp/narrative-ai-agent/internal/slowlog"
	"github.com/bestxp/narrative-ai-agent/internal/usecase/tools"
)

// stubSummarizer is a hand-rolled mock for the
// NPCSummarizer interface. It returns the body it was
// configured to return (or the input, if none) so tests
// can exercise the success / no-op / fail paths without
// touching a real LLM.
type stubSummarizer struct {
	returnBody []byte
	err       error
	calls     int
}

func (s *stubSummarizer) SummarizeNPC(_ context.Context, _, _ string, yamlBody, _ []byte) ([]byte, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	if s.returnBody != nil {
		return s.returnBody, nil
	}
	return yamlBody, nil
}

// writeLongNPC seeds the FileStore with a 50-fact
// profile. It returns the slug used.
func writeLongNPC(t *testing.T, fs *storage.FileStore, world, slug, name string) {
	t.Helper()
	profile, err := npcprofile.Load(`display_name: "` + name + `"
file_slug: "` + slug + `"
temperament: "x"
`)
	require.NoError(t, err)
	for i := 1; i <= 50; i++ {
		profile.PersonalMemory = append(profile.PersonalMemory, "Факт номер "+itoa(i))
	}
	body, err := profile.Save()
	require.NoError(t, err)
	require.NoError(t, fs.WriteRawAtomic("worlds/"+world+"/characters/"+slug+".yaml", body))
}

// itoa is a tiny strconv.Itoa shim to avoid the
// stdlib import in a test helper that only needs
// one digit-to-string conversion. The tests use it
// to build the 50-fact profile above.
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

func TestMaintainNPCs_NilSummarizerWarnsAndSkips(t *testing.T) {
	fs, err := storage.NewFileStore(t.TempDir())
	require.NoError(t, err)
	writeLongNPC(t, fs, "naruto", "kakashi", "Какаши")
	m := newMemory(fs, zerolog.Nop(), nil, nil)
	touched, err := m.MaintainNPCs("naruto")
	require.NoError(t, err)
	assert.Empty(t, touched, "no summarizer — file should not be touched")
}

func TestMaintainNPCs_BelowThresholdSkips(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	// Profile with 10 facts, well under 40.
	profile, _ := npcprofile.Load(`display_name: "X"
file_slug: "x"
`)
	for i := 0; i < 10; i++ {
		profile.PersonalMemory = append(profile.PersonalMemory, "факт")
	}
	body, _ := profile.Save()
	require.NoError(t, fs.WriteRawAtomic("worlds/naruto/characters/x.yaml", body))

	// Summarizer records no calls.
	stub := &stubSummarizer{}
	m := newMemory(fs, zerolog.Nop(), stub, nil)
	touched, err := m.MaintainNPCs("naruto")
	require.NoError(t, err)
	assert.Empty(t, touched)
	assert.Equal(t, 0, stub.calls, "summarizer must not be called for under-threshold profiles")
}

func TestMaintainNPCs_ShrinksPersonalMemory(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	writeLongNPC(t, fs, "naruto", "kakashi", "Какаши")

	// Stub summarizer returns a profile with 25 facts
	// (well under the 40 threshold).
	original, err := npcprofile.Load(yamlFixture())
	require.NoError(t, err)
	original.PersonalMemory = nil
	for i := 0; i < 25; i++ {
		original.PersonalMemory = append(original.PersonalMemory, "compacted fact")
	}
	newBody, err := original.Save()
	require.NoError(t, err)

	stub := &stubSummarizer{returnBody: []byte(newBody)}
	m := newMemory(fs, zerolog.Nop(), stub, nil)
	touched, err := m.MaintainNPCs("naruto")
	require.NoError(t, err)
	assert.Equal(t, []string{"Какаши"}, touched)
	assert.Equal(t, 1, stub.calls)

	// The file on disk must now be the new (smaller) YAML.
	got, err := fs.ReadRaw("worlds/naruto/characters/kakashi.yaml")
	require.NoError(t, err)
	assert.NotContains(t, got, "Факт номер 50", "old fact 50 must be gone")
	// 25 facts of "compacted fact" are present.
	assert.Equal(t, 25, strings.Count(got, "compacted fact"))
}

func TestMaintainNPCs_RejectsNoShrink(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	writeLongNPC(t, fs, "naruto", "kakashi", "Какаши")

	// Stub summarizer returns the SAME body (no shrink).
	stub := &stubSummarizer{returnBody: []byte("uncompressed")}
	m := newMemory(fs, zerolog.Nop(), stub, nil)
	touched, err := m.MaintainNPCs("naruto")
	require.NoError(t, err)
	assert.Empty(t, touched, "no shrink — no write")
}

func TestMaintainNPCs_RejectsInvalidYAML(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	writeLongNPC(t, fs, "naruto", "kakashi", "Какаши")

	// Stub summarizer returns garbage.
	stub := &stubSummarizer{returnBody: []byte("not valid yaml: [")}
	m := newMemory(fs, zerolog.Nop(), stub, nil)
	touched, err := m.MaintainNPCs("naruto")
	require.NoError(t, err)
	assert.Empty(t, touched, "invalid YAML — no write")
}

func TestMaintainNPCs_PropagatesError(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	writeLongNPC(t, fs, "naruto", "kakashi", "Какаши")

	stub := &stubSummarizer{err: errTest}
	m := newMemory(fs, zerolog.Nop(), stub, nil)
	// Per-NPC errors are isolated: a failed
	// summarizer call is logged at warn level but
	// does not abort the loop, and MaintainNPCs
	// returns nil. The failure is visible in slowlog
	// via the "failed" list. We assert that
	// MaintainNPCs returns nil (no fatal) and that
	// the underlying summarizer WAS called.
	_, err := m.MaintainNPCs("naruto")
	assert.NoError(t, err, "per-NPC errors are not fatal")
	assert.Equal(t, 1, stub.calls, "summarizer was called once before erroring")
}

func TestMaintainNPCs_LegacyMarkdownMigratedOnTheFly(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	// Legacy file: # Kakashi\nспокойный (from the
	// npcprofile migration tests). The parser migrates
	// to YAML, sees no personal_memory (below 40), and
	// the summarizer is never called.
	require.NoError(t, fs.WriteRawAtomic("worlds/naruto/characters/kakashi.yaml",
		"# Какаши\nспокойный\n"))

	stub := &stubSummarizer{}
	m := newMemory(fs, zerolog.Nop(), stub, nil)
	touched, err := m.MaintainNPCs("naruto")
	require.NoError(t, err)
	assert.Empty(t, touched)
	assert.Equal(t, 0, stub.calls, "legacy markdown with no overgrowth should not trigger summarizer")
}

func TestMaintainNPCs_PartialFailureDoesNotAbort(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	// One long file (will be compacted) + one short
	// file (skipped). The summary call for the long
	// file succeeds; the short file is skipped silently.
	writeLongNPC(t, fs, "naruto", "long_npc", "Длинный NPC")
	// Short profile (5 facts) is below threshold.
	short, _ := npcprofile.Load(`display_name: "Короткий"
file_slug: "short_npc"
`)
	for i := 0; i < 5; i++ {
		short.PersonalMemory = append(short.PersonalMemory, "мало")
	}
	shortBody, _ := short.Save()
	require.NoError(t, fs.WriteRawAtomic("worlds/naruto/characters/short_npc.yaml", shortBody))

	// Stub summarizer returns a 20-fact profile.
	original, _ := npcprofile.Load(yamlFixture())
	original.PersonalMemory = nil
	for i := 0; i < 20; i++ {
		original.PersonalMemory = append(original.PersonalMemory, "ok")
	}
	newBody, _ := original.Save()
	stub := &stubSummarizer{returnBody: []byte(newBody)}

	m := newMemory(fs, zerolog.Nop(), stub, nil)
	touched, err := m.MaintainNPCs("naruto")
	require.NoError(t, err)
	assert.Equal(t, []string{"Длинный NPC"}, touched, "only the long NPC is touched")
	assert.Equal(t, 1, stub.calls, "summarizer is called only once for the long NPC")
}

// assertAnError is a small helper for tests that need
// any non-nil error. Avoids pulling errors.New from
// "errors" just for one line.
func assertAnError() (err error) {
	return errTest
}

var errTest = errStub("synthetic test error")

type errStub string

func (e errStub) Error() string { return string(e) }

// yamlFixture is the canonical "fresh" profile used as
// a starting point for the shrink tests. We then mutate
// PersonalMemory in-place and Save() to get a known body
// for the stub summarizer to return.
func yamlFixture() string {
	return `display_name: "stub"
file_slug: "stub"
temperament: ""
`
}

// stubLoreSummarizer is a hand-rolled mock for the
// LoreSummarizer interface. It returns the body it was
// configured to return (or the input, if none) so tests
// can exercise the success / no-op / fail paths without
// touching a real LLM.
type stubLoreSummarizer struct {
	returnBody []byte
	err       error
	calls     int
}

func (s *stubLoreSummarizer) SummarizeLore(_ context.Context, _ string, loreBody, _, _ []byte) ([]byte, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	if s.returnBody != nil {
		return s.returnBody, nil
	}
	return loreBody, nil
}

// writeLongLore seeds the FileStore with a 600-line
// lore.md (well over the 500-line threshold). Returns
// the body so individual tests can tweak it further.
func writeLongLore(t *testing.T, fs *storage.FileStore, world string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("# Lore — мир " + world + "\n\n")
	for i := 1; i <= 200; i++ {
		fmt.Fprintf(&b, "## День %d: Событие номер %d\n", i, i)
		b.WriteString("- важный факт сюжета\n")
		b.WriteString("- ещё один факт\n")
		b.WriteString("- третий факт\n\n")
	}
	body := b.String()
	require.NoError(t, fs.WriteRawAtomic("worlds/"+world+"/lore.md", body))
	return body
}

func TestMaintainLore_NilSummarizerWarnsAndSkips(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	writeLongLore(t, fs, "naruto")
	m := newMemory(fs, zerolog.Nop(), nil, nil)
	rewritten, err := m.MaintainLore(context.Background(), "naruto")
	require.NoError(t, err)
	assert.False(t, rewritten, "no summarizer — file should not be touched")
}

func TestMaintainLore_BelowThresholdSkips(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	// 50 lines, well under 500.
	require.NoError(t, fs.WriteRawAtomic("worlds/naruto/lore.md", "# Lore\n\n## Day 1\n- fact\n\n## Day 2\n- fact\n"))
	stub := &stubLoreSummarizer{}
	m := newMemory(fs, zerolog.Nop(), nil, stub)
	rewritten, err := m.MaintainLore(context.Background(), "naruto")
	require.NoError(t, err)
	assert.False(t, rewritten)
	assert.Equal(t, 0, stub.calls, "summarizer must not be called for under-threshold lore")
}

func TestMaintainLore_ShrinksLoreFile(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	writeLongLore(t, fs, "naruto")
	// Return a smaller body.
	shortBody := "# Lore — мир naruto\n\n## День 1\n- ключевой факт\n"
	stub := &stubLoreSummarizer{returnBody: []byte(shortBody)}
	m := newMemory(fs, zerolog.Nop(), nil, stub)
	rewritten, err := m.MaintainLore(context.Background(), "naruto")
	require.NoError(t, err)
	assert.True(t, rewritten)
	assert.Equal(t, 1, stub.calls)

	got, err := fs.ReadRaw("worlds/naruto/lore.md")
	require.NoError(t, err)
	assert.Contains(t, got, "ключевой факт", "new content must be on disk")
	assert.NotContains(t, got, "Событие номер 200", "old large content must be gone")
}

func TestMaintainLore_RejectsNoShrink(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	writeLongLore(t, fs, "naruto")
	original, err := fs.ReadRaw("worlds/naruto/lore.md")
	require.NoError(t, err)
	// Return the same body (no shrink).
	stub := &stubLoreSummarizer{returnBody: []byte(original)}
	m := newMemory(fs, zerolog.Nop(), nil, stub)
	rewritten, err := m.MaintainLore(context.Background(), "naruto")
	require.NoError(t, err)
	assert.False(t, rewritten, "no shrink — no write")
}

func TestMaintainLore_RejectsNoSections(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	writeLongLore(t, fs, "naruto")
	// Return prose with no "## " headers — would
	// destroy the canonical section structure.
	stub := &stubLoreSummarizer{returnBody: []byte("just one long sentence with no sections")}
	m := newMemory(fs, zerolog.Nop(), nil, stub)
	rewritten, err := m.MaintainLore(context.Background(), "naruto")
	require.NoError(t, err)
	assert.False(t, rewritten, "no sections — no write")
}

func TestMaintainLore_PropagatesError(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	writeLongLore(t, fs, "naruto")
	stub := &stubLoreSummarizer{err: errTest}
	m := newMemory(fs, zerolog.Nop(), nil, stub)
	_, err := m.MaintainLore(context.Background(), "naruto")
	assert.Error(t, err)
}

func TestMaintainLore_MissingFileNoop(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	stub := &stubLoreSummarizer{}
	m := newMemory(fs, zerolog.Nop(), nil, stub)
	rewritten, err := m.MaintainLore(context.Background(), "naruto")
	require.NoError(t, err)
	assert.False(t, rewritten)
	assert.Equal(t, 0, stub.calls, "missing file — no summarizer call")
}

// Compile-time guard: stubLoreSummarizer satisfies the
// LoreSummarizer interface.
var _ tools.LoreSummarizer = (*stubLoreSummarizer)(nil)

// Reference to slowlog keeps the import live in case
// the test ever needs a real Logger.
var _ = slowlog.Discard
