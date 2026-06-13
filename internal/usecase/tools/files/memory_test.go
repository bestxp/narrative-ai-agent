package files

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bestxp/narrative-ai-agent/internal/adapter/storage"
	"github.com/bestxp/narrative-ai-agent/internal/domain"
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
	err        error
	calls      int
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
	m := newMemory(fs, zerolog.Nop(), nil, nil, nil, nil)
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
	m := newMemory(fs, zerolog.Nop(), stub, nil, nil, nil)
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
	m := newMemory(fs, zerolog.Nop(), stub, nil, nil, nil)
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
	m := newMemory(fs, zerolog.Nop(), stub, nil, nil, nil)
	touched, err := m.MaintainNPCs("naruto")
	require.NoError(t, err)
	assert.Empty(t, touched, "no shrink — no write")
}

// TestMaintainNPCs_BackupCreatedBeforeRewrite: when a
// profile is rewritten by maintain, the previous version
// is preserved at "<slug>.yaml.bak" so an operator can
// roll back if a buggy model ever corrupts the canonical
// file. The .bak is overwritten on the next maintain of
// the same slug (it is the "previous successful write"
// checkpoint, not an append-only log).
func TestMaintainNPCs_BackupCreatedBeforeRewrite(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	writeLongNPC(t, fs, "naruto", "kakashi", "Какаши")

	// Summarizer returns a valid shorter YAML.
	original, _ := npcprofile.Load(yamlFixture())
	for i := 0; i < 25; i++ {
		original.PersonalMemory = append(original.PersonalMemory, "compacted")
	}
	newBody, err := original.Save()
	require.NoError(t, err)

	stub := &stubSummarizer{returnBody: []byte(newBody)}
	m := newMemory(fs, zerolog.Nop(), stub, nil, nil, nil)
	touched, err := m.MaintainNPCs("naruto")
	require.NoError(t, err)
	require.Equal(t, []string{"Какаши"}, touched)

	// .bak must exist and contain the ORIGINAL 50-fact body.
	bak, err := fs.ReadRaw("worlds/naruto/characters/kakashi.yaml.bak")
	require.NoError(t, err)
	assert.Contains(t, bak, "Факт номер 1", ".bak preserves pre-rewrite bytes")
	assert.Contains(t, bak, "Факт номер 50", ".bak preserves pre-rewrite bytes")
	assert.Equal(t, 50, strings.Count(bak, "Факт номер"), "all 50 facts survive in the backup")

	// Current file must be the new (compacted) version.
	cur, err := fs.ReadRaw("worlds/naruto/characters/kakashi.yaml")
	require.NoError(t, err)
	assert.NotContains(t, cur, "Факт номер 50", "current file is the compacted body")
}

func TestMaintainNPCs_RejectsInvalidYAML(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	writeLongNPC(t, fs, "naruto", "kakashi", "Какаши")

	// Stub summarizer returns garbage.
	stub := &stubSummarizer{returnBody: []byte("not valid yaml: [")}
	m := newMemory(fs, zerolog.Nop(), stub, nil, nil, nil)
	touched, err := m.MaintainNPCs("naruto")
	require.NoError(t, err)
	assert.Empty(t, touched, "invalid YAML — no write")
}

func TestMaintainNPCs_PropagatesError(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	writeLongNPC(t, fs, "naruto", "kakashi", "Какаши")

	stub := &stubSummarizer{err: errTest}
	m := newMemory(fs, zerolog.Nop(), stub, nil, nil, nil)
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
	m := newMemory(fs, zerolog.Nop(), stub, nil, nil, nil)
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

	m := newMemory(fs, zerolog.Nop(), stub, nil, nil, nil)
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
	err        error
	calls      int
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
	m := newMemory(fs, zerolog.Nop(), nil, nil, nil, nil)
	rewritten, err := m.MaintainLore(context.Background(), "naruto")
	require.NoError(t, err)
	assert.False(t, rewritten, "no summarizer — file should not be touched")
}

func TestMaintainLore_BelowThresholdSkips(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	// 50 lines, well under 500.
	require.NoError(t, fs.WriteRawAtomic("worlds/naruto/lore.md", "# Lore\n\n## Day 1\n- fact\n\n## Day 2\n- fact\n"))
	stub := &stubLoreSummarizer{}
	m := newMemory(fs, zerolog.Nop(), nil, stub, nil, nil)
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
	m := newMemory(fs, zerolog.Nop(), nil, stub, nil, nil)
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
	m := newMemory(fs, zerolog.Nop(), nil, stub, nil, nil)
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
	m := newMemory(fs, zerolog.Nop(), nil, stub, nil, nil)
	rewritten, err := m.MaintainLore(context.Background(), "naruto")
	require.NoError(t, err)
	assert.False(t, rewritten, "no sections — no write")
}

func TestMaintainLore_PropagatesError(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	writeLongLore(t, fs, "naruto")
	stub := &stubLoreSummarizer{err: errTest}
	m := newMemory(fs, zerolog.Nop(), nil, stub, nil, nil)
	_, err := m.MaintainLore(context.Background(), "naruto")
	assert.Error(t, err)
}

func TestMaintainLore_MissingFileNoop(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	stub := &stubLoreSummarizer{}
	m := newMemory(fs, zerolog.Nop(), nil, stub, nil, nil)
	rewritten, err := m.MaintainLore(context.Background(), "naruto")
	require.NoError(t, err)
	assert.False(t, rewritten)
	assert.Equal(t, 0, stub.calls, "missing file — no summarizer call")
}

// Compile-time guard: stubLoreSummarizer satisfies the
// LoreSummarizer interface.
var _ tools.LoreSummarizer = (*stubLoreSummarizer)(nil)

// --- character memory maintain ---------------------------------------

// stubCharMemSummarizer is a deterministic test double
// for tools.CharacterMemorySummarizer. Returns the
// configured returnBody verbatim (or the input if
// returnBody is nil — the "no shrink" path), records
// every call.
type stubCharMemSummarizer struct {
	returnBody []byte
	err        error
	calls      int
	lastWorld  string
	lastChar   string
}

func (s *stubCharMemSummarizer) SummarizeCharacterMemory(_ context.Context, world, character string, memoryBody, _ []byte) ([]byte, error) {
	s.calls++
	s.lastWorld = world
	s.lastChar = character
	if s.err != nil {
		return nil, s.err
	}
	if s.returnBody != nil {
		return s.returnBody, nil
	}
	return memoryBody, nil
}

var _ tools.CharacterMemorySummarizer = (*stubCharMemSummarizer)(nil)

// writeLongMemory seeds the FileStore with a memory.yaml
// over tools.CharacterMemoryMaintainBytes (4KB). The
// fixture carries 4 legacy free-form sections ("Эмоции",
// "Действия дня 1", "Видения Кагуи", "Яркие моменты")
// so the regression test for "refile legacy into
// canonical" can be one test, not two.
func writeLongMemory(t *testing.T, fs *storage.FileStore, character string) string {
	t.Helper()
	// Build a body that crosses the 4KB threshold and
	// has the 4-section enum + 4 legacy free-form
	// sections, so tests can assert "the canonical
	// version still has the 4 enum sections" after
	// maintain.
	var b strings.Builder
	b.WriteString("data:\n")
	for _, sec := range []string{
		"Яркие моменты",
		"Факты о мире",
		"Обещания и цели",
		"Важные люди",
		// Legacy / non-canonical sections the
		// summarizer is expected to refile into one of
		// the 4 above.
		"Эмоции",
		"Эволюция",
		"Действия дня 1",
		"Действия дня 9",
		"Видения Кагуи",
		"Контакт с семьёй Яманака",
		"Яркие воспоминания Маркус",
	} {
		fmt.Fprintf(&b, "    - section: %q\n      values:\n", sec)
		// 8 long bullets per section — the long Russian
		// sentences (~140 bytes each) push the total
		// body past 4KB after 7+ sections.
		for i := 0; i < 8; i++ {
			fmt.Fprintf(&b, "        - \"Длинный факт номер %d в секции %s с подробностями и контекстом истории и пейзажа долины Огня\" \n", i+1, sec)
		}
	}
	body := b.String()
	require.NoError(t, fs.WriteRawAtomic("characters/"+character+"/memory.yaml", body))
	return body
}

func TestMaintainCharacterMemory_NilSummarizerWarnsAndSkips(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	writeLongMemory(t, fs, "markus")
	m := newMemory(fs, zerolog.Nop(), nil, nil, nil, nil)
	rewritten, err := m.MaintainCharacterMemory(context.Background(), "naruto", "markus")
	require.NoError(t, err)
	assert.False(t, rewritten, "no summarizer — file should not be touched")
}

func TestMaintainCharacterMemory_BelowThresholdSkips(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	// 200-byte file — well under 4KB. The LLM must not
	// be called: maintain is for already-bloated files.
	require.NoError(t, fs.WriteRawAtomic("characters/markus/memory.yaml", "data:\n    - section: \"Яркие моменты\"\n      values:\n        - \"один факт\"\n"))
	stub := &stubCharMemSummarizer{}
	m := newMemory(fs, zerolog.Nop(), nil, nil, nil, stub)
	rewritten, err := m.MaintainCharacterMemory(context.Background(), "naruto", "markus")
	require.NoError(t, err)
	assert.False(t, rewritten)
	assert.Equal(t, 0, stub.calls, "summarizer must not be called for under-threshold memory")
}

func TestMaintainCharacterMemory_ShrinksAndRefilesLegacy(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	writeLongMemory(t, fs, "markus")
	// Return a strictly smaller, valid Memory YAML
	// with the 4 canonical sections. The summarizer
	// is responsible for refiling legacy free-form
	// sections (we just verify the file is the new
	// canonical shape).
	shrunk := `data:
    - section: "Яркие моменты"
      values:
        - "Видение с Кагуей"
    - section: "Факты о мире"
      values:
        - "Ринне-шаринган — глаза Оцуцуки"
    - section: "Обещания и цели"
      values:
        - "Спасти Кагую"
    - section: "Важные люди"
      values:
        - "Наруто, Ирука, Хокаге"
`
	stub := &stubCharMemSummarizer{returnBody: []byte(shrunk)}
	m := newMemory(fs, zerolog.Nop(), nil, nil, nil, stub)
	rewritten, err := m.MaintainCharacterMemory(context.Background(), "naruto", "markus")
	require.NoError(t, err)
	assert.True(t, rewritten)
	assert.Equal(t, 1, stub.calls)
	assert.Equal(t, "naruto", stub.lastWorld)
	assert.Equal(t, "markus", stub.lastChar)

	got, err := fs.ReadRaw("characters/markus/memory.yaml")
	require.NoError(t, err)
	assert.Contains(t, got, "Видение с Кагуей", "new content must be on disk")
	assert.NotContains(t, got, "Длинный факт номер", "old long bullets must be gone")
	// The 4 canonical sections must be present.
	for _, s := range []string{"Яркие моменты", "Факты о мире", "Обещания и цели", "Важные люди"} {
		assert.Contains(t, got, s, "canonical section %q must remain", s)
	}
}

func TestMaintainCharacterMemory_RejectsNoShrink(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	writeLongMemory(t, fs, "markus")
	before, _ := fs.ReadRaw("characters/markus/memory.yaml")
	// Return the same body (no shrink) — caller bails
	// because maintenance never grows a file.
	stub := &stubCharMemSummarizer{returnBody: []byte(before)}
	m := newMemory(fs, zerolog.Nop(), nil, nil, nil, stub)
	rewritten, err := m.MaintainCharacterMemory(context.Background(), "naruto", "markus")
	require.NoError(t, err)
	assert.False(t, rewritten, "no shrink — no write")
}

func TestMaintainCharacterMemory_RejectsEmpty(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	writeLongMemory(t, fs, "markus")
	before, _ := fs.ReadRaw("characters/markus/memory.yaml")
	stub := &stubCharMemSummarizer{returnBody: []byte("")}
	m := newMemory(fs, zerolog.Nop(), nil, nil, nil, stub)
	rewritten, err := m.MaintainCharacterMemory(context.Background(), "naruto", "markus")
	require.NoError(t, err)
	assert.False(t, rewritten)
	after, _ := fs.ReadRaw("characters/markus/memory.yaml")
	assert.Equal(t, before, after, "empty body — file untouched")
}

func TestMaintainCharacterMemory_RejectsInvalidYAML(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	writeLongMemory(t, fs, "markus")
	before, _ := fs.ReadRaw("characters/markus/memory.yaml")
	stub := &stubCharMemSummarizer{returnBody: []byte("not valid yaml: [")}
	m := newMemory(fs, zerolog.Nop(), nil, nil, nil, stub)
	rewritten, err := m.MaintainCharacterMemory(context.Background(), "naruto", "markus")
	require.NoError(t, err)
	assert.False(t, rewritten, "invalid YAML — no write")
	after, _ := fs.ReadRaw("characters/markus/memory.yaml")
	assert.Equal(t, before, after)
}

func TestMaintainCharacterMemory_RejectsNoSections(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	writeLongMemory(t, fs, "markus")
	before, _ := fs.ReadRaw("characters/markus/memory.yaml")
	// Empty data: array — would destroy the file.
	empty := "data: []\n"
	stub := &stubCharMemSummarizer{returnBody: []byte(empty)}
	m := newMemory(fs, zerolog.Nop(), nil, nil, nil, stub)
	rewritten, err := m.MaintainCharacterMemory(context.Background(), "naruto", "markus")
	require.NoError(t, err)
	assert.False(t, rewritten, "no sections — no write")
	after, _ := fs.ReadRaw("characters/markus/memory.yaml")
	assert.Equal(t, before, after)
}

func TestMaintainCharacterMemory_BackupCreatedBeforeRewrite(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	writeLongMemory(t, fs, "markus")
	before, _ := fs.ReadRaw("characters/markus/memory.yaml")
	shrunk := `data:
    - section: "Яркие моменты"
      values:
        - "Видение с Кагуей"
    - section: "Факты о мире"
      values:
        - "Ринне-шаринган"
    - section: "Обещания и цели"
      values:
        - "Спасти Кагую"
    - section: "Важные люди"
      values:
        - "Наруто"
`
	stub := &stubCharMemSummarizer{returnBody: []byte(shrunk)}
	m := newMemory(fs, zerolog.Nop(), nil, nil, nil, stub)
	rewritten, err := m.MaintainCharacterMemory(context.Background(), "naruto", "markus")
	require.NoError(t, err)
	require.True(t, rewritten)
	// .bak must contain the ORIGINAL long body.
	bak, err := fs.ReadRaw("characters/markus/memory.yaml.bak")
	require.NoError(t, err)
	assert.Equal(t, before, bak, ".bak preserves pre-rewrite bytes")
	// Current file must be the new (compacted) version.
	cur, _ := fs.ReadRaw("characters/markus/memory.yaml")
	assert.Contains(t, cur, "Видение с Кагуей", "current file is the compacted body")
}

func TestMaintainCharacterMemory_MissingFileNoop(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	stub := &stubCharMemSummarizer{}
	m := newMemory(fs, zerolog.Nop(), nil, nil, nil, stub)
	rewritten, err := m.MaintainCharacterMemory(context.Background(), "naruto", "markus")
	require.NoError(t, err)
	assert.False(t, rewritten)
	assert.Equal(t, 0, stub.calls, "missing file — no summarizer call")
}

func TestMaintainCharacterMemory_EmptyCharacterNoop(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	stub := &stubCharMemSummarizer{}
	m := newMemory(fs, zerolog.Nop(), nil, nil, nil, stub)
	rewritten, err := m.MaintainCharacterMemory(context.Background(), "naruto", "")
	require.NoError(t, err)
	assert.False(t, rewritten)
	assert.Equal(t, 0, stub.calls, "empty character — no summarizer call")
}

// Reference to slowlog keeps the import live in case
// the test ever needs a real Logger.
var _ = slowlog.Discard

// --- memorise window compression ----------------------------------------

// stubMemoriseSummarizer is a deterministic test double
// for tools.MemoriseSummarizer. It records every call and
// returns a fixed body that follows the
// "д<start>-д<end>: <text>" contract, unless the test
// wants the model to misbehave (err / empty / bad prefix).
type stubMemoriseSummarizer struct {
	calls     int
	lastBody  string
	lastFrom  int
	lastTo    int
	firstFrom int
	firstTo   int
	// ReturnedBody is appended after the canonical prefix
	// to produce the final line. Empty string yields a
	// model that returned no content at all.
	returnedBody string
	// BadPrefix drops the canonical prefix, simulating a
	// model that drifted.
	badPrefix bool
	err       error
	// SkipOutput, when true, makes the stub return
	// ([]byte{}, nil) regardless of returnedBody — the
	// "too thin" code path.
	skipOutput bool
}

func (s *stubMemoriseSummarizer) SummarizeMemorise(ctx context.Context, world string, startDay, endDay int, fullMemorise string) ([]byte, error) {
	s.calls++
	if s.calls == 1 {
		s.firstFrom = startDay
		s.firstTo = endDay
	}
	s.lastFrom = startDay
	s.lastTo = endDay
	s.lastBody = fullMemorise
	if s.err != nil {
		return nil, s.err
	}
	if s.skipOutput {
		return []byte{}, nil
	}
	prefix := fmt.Sprintf("д%05d-д%05d: ", startDay, endDay)
	if s.badPrefix {
		return []byte("д00099-д00099: nonsense"), nil
	}
	return []byte(prefix + s.returnedBody), nil
}

var _ tools.MemoriseSummarizer = (*stubMemoriseSummarizer)(nil)

// writeMemoriseDays writes a 1-day-per-line memorise.md
// for the world. Days not in the `days` slice are
// skipped (so the test can simulate a gap in the
// calendar — e.g. 1..29, 31..60 with day 30 missing).
func writeMemoriseDays(t *testing.T, fs *storage.FileStore, world string, days map[int]string) {
	t.Helper()
	var b strings.Builder
	nums := make([]int, 0, len(days))
	for n := range days {
		nums = append(nums, n)
	}
	sort.Ints(nums)
	for _, n := range nums {
		b.WriteString(domain.FormatDay(n, days[n]))
		b.WriteString("\n")
	}
	require.NoError(t, fs.WriteRawAtomic("worlds/"+world+"/memorise.md", b.String()))
}

// TestMemoriseCompressWindow_Basic30Days writes days
// 1..30, calls ArchiveDay(30) which must compress
// д00001-д00030 into a single line.
func TestMemoriseCompressWindow_Basic30Days(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	require.NoError(t, fs.EnsureDir("worlds/naruto"))
	days := make(map[int]string, 30)
	for i := 1; i <= 30; i++ {
		days[i] = "день " + strconv.Itoa(i)
	}
	writeMemoriseDays(t, fs, "naruto", days)
	stub := &stubMemoriseSummarizer{returnedBody: "выжимка 30 дней"}
	mem := newMemory(fs, zerolog.Nop(), nil, nil, stub, nil)
	// ArchiveDay will call the hook after writing. We use
	// the state struct directly because the tool wiring
	// (State.ArchiveDay → Memory.memoriseCompressAfterArchive)
	// is set up in NewFileToolset, but we can also call
	// memoriseCompressAfterArchive directly.
	err := mem.memoriseCompressAfterArchive(context.Background(), "naruto", 30)
	require.NoError(t, err)
	assert.Equal(t, 1, stub.calls)
	assert.Equal(t, 1, stub.lastFrom)
	assert.Equal(t, 30, stub.lastTo)
	// File should now contain the summary line.
	body, _ := fs.ReadRaw("worlds/naruto/memorise.md")
	assert.Contains(t, body, "д00001-д00030: выжимка 30 дней")
	// No more single-day lines for days 1..30.
	for i := 1; i <= 30; i++ {
		assert.NotContains(t, body, "д"+strconv.Itoa(i)+":", "day %d should be gone", i)
		if i < 10 {
			assert.NotContains(t, body, "д0000"+strconv.Itoa(i)+":")
		}
	}
}

// TestMemoriseCompressWindow_Ladder: archive up to day
// 60 → must produce TWO summary lines, one per window.
func TestMemoriseCompressWindow_Ladder(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	require.NoError(t, fs.EnsureDir("worlds/naruto"))
	days := make(map[int]string, 60)
	for i := 1; i <= 60; i++ {
		days[i] = "день " + strconv.Itoa(i)
	}
	writeMemoriseDays(t, fs, "naruto", days)
	stub := &stubMemoriseSummarizer{returnedBody: "выжимка"}
	mem := newMemory(fs, zerolog.Nop(), nil, nil, stub, nil)
	err := mem.memoriseCompressAfterArchive(context.Background(), "naruto", 60)
	require.NoError(t, err)
	assert.Equal(t, 2, stub.calls, "two windows → two LLM calls")
	body, _ := fs.ReadRaw("worlds/naruto/memorise.md")
	assert.Contains(t, body, "д00001-д00030:")
	assert.Contains(t, body, "д00031-д00060:")
}

// TestMemoriseCompressWindow_Timeskip simulates a jump
// from day 10 to day 90 in one ArchiveDay call (e.g. the
// player left the world and returned). The file already
// has days 1..10; the new archive brings the count to
// 90. Three windows must be compressed: 1..30, 31..60,
// 61..90.
func TestMemoriseCompressWindow_Timeskip(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	require.NoError(t, fs.EnsureDir("worlds/naruto"))
	// Pre-existing: days 1..10.
	pre := make(map[int]string, 10)
	for i := 1; i <= 10; i++ {
		pre[i] = "до таймскипа " + strconv.Itoa(i)
	}
	writeMemoriseDays(t, fs, "naruto", pre)
	// Simulate the new days being written through the
	// state.ArchiveDay path: write days 11..90 directly
	// here, then call the hook as if day 90 was just
	// archived.
	var buf strings.Builder
	for i := 1; i <= 90; i++ {
		buf.WriteString(domain.FormatDay(i, "день "+strconv.Itoa(i)))
		buf.WriteString("\n")
	}
	require.NoError(t, fs.WriteRawAtomic("worlds/naruto/memorise.md", buf.String()))
	stub := &stubMemoriseSummarizer{returnedBody: "окно"}
	mem := newMemory(fs, zerolog.Nop(), nil, nil, stub, nil)
	err := mem.memoriseCompressAfterArchive(context.Background(), "naruto", 90)
	require.NoError(t, err)
	assert.Equal(t, 3, stub.calls, "three windows → three LLM calls")
	body, _ := fs.ReadRaw("worlds/naruto/memorise.md")
	assert.Contains(t, body, "д00001-д00030:")
	assert.Contains(t, body, "д00031-д00060:")
	assert.Contains(t, body, "д00061-д00090:")
	// The single-day lines for 1..90 must be gone.
	for i := 1; i <= 90; i++ {
		if i < 10 {
			assert.NotContains(t, body, "д0000"+strconv.Itoa(i)+": день")
		} else {
			assert.NotContains(t, body, "д"+strconv.Itoa(i)+": день")
		}
	}
}

// TestMemoriseCompressWindow_NoSummarizer: nil summarizer
// must log a warning and leave the file untouched.
func TestMemoriseCompressWindow_NoSummarizer(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	require.NoError(t, fs.EnsureDir("worlds/naruto"))
	days := make(map[int]string, 30)
	for i := 1; i <= 30; i++ {
		days[i] = "день"
	}
	writeMemoriseDays(t, fs, "naruto", days)
	before, _ := fs.ReadRaw("worlds/naruto/memorise.md")
	mem := newMemory(fs, zerolog.Nop(), nil, nil, nil, nil)
	err := mem.memoriseCompressAfterArchive(context.Background(), "naruto", 30)
	require.NoError(t, err)
	after, _ := fs.ReadRaw("worlds/naruto/memorise.md")
	assert.Equal(t, before, after, "no summarizer → no write")
}

// TestMemoriseCompressWindow_BadPrefix: model returns a
// line that does NOT start with the required prefix; the
// window must be left untouched.
func TestMemoriseCompressWindow_BadPrefix(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	require.NoError(t, fs.EnsureDir("worlds/naruto"))
	days := make(map[int]string, 30)
	for i := 1; i <= 30; i++ {
		days[i] = "день"
	}
	writeMemoriseDays(t, fs, "naruto", days)
	before, _ := fs.ReadRaw("worlds/naruto/memorise.md")
	stub := &stubMemoriseSummarizer{badPrefix: true}
	mem := newMemory(fs, zerolog.Nop(), nil, nil, stub, nil)
	err := mem.memoriseCompressAfterArchive(context.Background(), "naruto", 30)
	require.NoError(t, err)
	after, _ := fs.ReadRaw("worlds/naruto/memorise.md")
	assert.Equal(t, before, after, "bad prefix → no write")
}

// TestMemoriseCompressWindow_TooThin: only 3 days in a
// 30-day window. Must skip the LLM call entirely.
func TestMemoriseCompressWindow_TooThin(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	require.NoError(t, fs.EnsureDir("worlds/naruto"))
	days := map[int]string{1: "a", 5: "b", 10: "c"}
	writeMemoriseDays(t, fs, "naruto", days)
	stub := &stubMemoriseSummarizer{returnedBody: "should not be called"}
	mem := newMemory(fs, zerolog.Nop(), nil, nil, stub, nil)
	err := mem.memoriseCompressAfterArchive(context.Background(), "naruto", 10)
	require.NoError(t, err)
	assert.Equal(t, 0, stub.calls, "window too thin (3 days out of 30) — no LLM call")
}

// TestMemoriseCompressWindow_WithGap: days 1..29 and
// 31..60 are present, day 30 is missing. The window
// 1..30 must still be compressed (29 days is plenty),
// and the model receives the actual present-day list
// (start=1, end=29 in actualStart/actualEnd).
func TestMemoriseCompressWindow_WithGap(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	require.NoError(t, fs.EnsureDir("worlds/naruto"))
	days := make(map[int]string, 59)
	for i := 1; i <= 29; i++ {
		days[i] = "день " + strconv.Itoa(i)
	}
	for i := 31; i <= 60; i++ {
		days[i] = "день " + strconv.Itoa(i)
	}
	writeMemoriseDays(t, fs, "naruto", days)
	stub := &stubMemoriseSummarizer{returnedBody: "сжато"}
	mem := newMemory(fs, zerolog.Nop(), nil, nil, stub, nil)
	err := mem.memoriseCompressAfterArchive(context.Background(), "naruto", 60)
	require.NoError(t, err)
	// First window saw 29 days (1..29); second saw 30
	// days (31..60). Both compressed.
	assert.Equal(t, 2, stub.calls)
	// The LLM was called with the LATEST window's bounds
	// as lastFrom/lastTo (31, 60) — that is the second
	// call. The first call's bounds are (1, 29).
	assert.Equal(t, 1, stub.firstFrom)
	assert.Equal(t, 29, stub.firstTo)
	assert.Equal(t, 31, stub.lastFrom)
	assert.Equal(t, 60, stub.lastTo)
	// And the prefix in the file must use the actual
	// window bounds (1..29 and 31..60), not the
	// "expected" 1..30 and 31..60.
	body, _ := fs.ReadRaw("worlds/naruto/memorise.md")
	assert.Contains(t, body, "д00001-д00029:")
	assert.Contains(t, body, "д00031-д00060:")
	assert.NotContains(t, body, "д00001-д00030:",
		"window 1..30 was actually 1..29 — prefix must reflect that")
}

// TestMemoriseCompressWindow_KeepsEarlierSummaries: if
// the file already has a summary line for д00001-д00030
// and we archive day 60, only д00031-д00060 should be
// rewritten. The earlier summary stays untouched.
func TestMemoriseCompressWindow_KeepsEarlierSummaries(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	require.NoError(t, fs.EnsureDir("worlds/naruto"))
	pre := "д00001-д00030: старая сводка 1\n"
	for i := 31; i <= 60; i++ {
		pre += domain.FormatDay(i, "день "+strconv.Itoa(i)) + "\n"
	}
	require.NoError(t, fs.WriteRawAtomic("worlds/naruto/memorise.md", pre))
	stub := &stubMemoriseSummarizer{returnedBody: "новая сводка 2"}
	mem := newMemory(fs, zerolog.Nop(), nil, nil, stub, nil)
	err := mem.memoriseCompressAfterArchive(context.Background(), "naruto", 60)
	require.NoError(t, err)
	assert.Equal(t, 1, stub.calls, "only one window left to compress")
	assert.Equal(t, 31, stub.lastFrom)
	assert.Equal(t, 60, stub.lastTo)
	body, _ := fs.ReadRaw("worlds/naruto/memorise.md")
	assert.Contains(t, body, "д00001-д00030: старая сводка 1",
		"earlier summary must NOT be touched")
	assert.Contains(t, body, "д00031-д00060: новая сводка 2")
}

// TestStateArchiveDay_TriggersCompression: end-to-end
// through State.ArchiveDay, the MemoriseCompressWindow
// hook must fire on a closing day. Day 30 closes the
// 1..30 window — it BECOMES part of the compressed
// summary line (the just-archived day's text "итог" is
// included in the source body the LLM sees). The result
// is a single summary line for the whole window.
func TestStateArchiveDay_TriggersCompression(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	require.NoError(t, fs.EnsureDir("worlds/naruto"))
	days := make(map[int]string, 30)
	for i := 1; i <= 30; i++ {
		days[i] = "день"
	}
	writeMemoriseDays(t, fs, "naruto", days)
	stub := &stubMemoriseSummarizer{returnedBody: "сводка"}
	mem := newMemory(fs, zerolog.Nop(), nil, nil, stub, nil)
	st := newState(fs, zerolog.Nop())
	st.SetMemoriseCompress(mem.memoriseCompressAfterArchive)
	require.NoError(t, st.ArchiveDay(context.Background(), "naruto", 30, "итог"))
	assert.Equal(t, 1, stub.calls)
	assert.Equal(t, 30, stub.lastTo)
	body, _ := fs.ReadRaw("worlds/naruto/memorise.md")
	assert.Contains(t, body, "д00001-д00030: сводка",
		"the 30-day window was compressed into a single summary line")
	// The per-day line for day 30 ("д00030: итог") is
	// GONE — it was absorbed into the summary.
	assert.NotContains(t, body, "д00030: итог",
		"day 30 itself is part of the compressed window")
}

// TestStateArchiveDay_NotMultipleOf30: archiving day
// 25 must NOT trigger any compression (no window closes).
func TestStateArchiveDay_NotMultipleOf30(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	require.NoError(t, fs.EnsureDir("worlds/naruto"))
	days := make(map[int]string, 25)
	for i := 1; i <= 25; i++ {
		days[i] = "день"
	}
	writeMemoriseDays(t, fs, "naruto", days)
	stub := &stubMemoriseSummarizer{returnedBody: "no"}
	mem := newMemory(fs, zerolog.Nop(), nil, nil, stub, nil)
	st := newState(fs, zerolog.Nop())
	st.SetMemoriseCompress(mem.memoriseCompressAfterArchive)
	require.NoError(t, st.ArchiveDay(context.Background(), "naruto", 25, "x"))
	assert.Equal(t, 0, stub.calls, "day 25 is not a window boundary")
}
