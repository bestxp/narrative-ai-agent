package files

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bestxp/narrative-ai-agent/internal/adapter/storage"
	"github.com/bestxp/narrative-ai-agent/internal/chronicle"
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

// --- chronicle window compression ----------------------------------------

// stubChronicleSummarizer is a deterministic test double
// for tools.ChronicleSummarizer. It records every call and
// returns a fixed memory text (the new Period's Memory
// field). Tests that want the model to misbehave set
// err / skipOutput.
type stubChronicleSummarizer struct {
	calls     int
	lastBody  string
	lastFrom  int
	lastTo    int
	firstFrom int
	firstTo   int
	// ReturnedMemory is what the stub returns as the
	// Period's Memory field. Empty string yields a model
	// that returned no content at all.
	returnedMemory string
	// Err, when set, makes the stub return an error.
	err error
	// SkipOutput, when true, makes the stub return
	// ([]byte{}, nil) regardless of returnedMemory — the
	// "too thin" code path.
	skipOutput bool
}

func (s *stubChronicleSummarizer) SummarizeChronicle(ctx context.Context, world string, startDay, endDay int, fullChronicle string) ([]byte, error) {
	s.calls++
	if s.calls == 1 {
		s.firstFrom = startDay
		s.firstTo = endDay
	}
	s.lastFrom = startDay
	s.lastTo = endDay
	s.lastBody = fullChronicle
	if s.err != nil {
		return nil, s.err
	}
	if s.skipOutput {
		return []byte{}, nil
	}
	return []byte(s.returnedMemory), nil
}

var _ tools.ChronicleSummarizer = (*stubChronicleSummarizer)(nil)

// writeChronicleDays writes a chronicle.yaml with the
// given raw per-day entries. The function is the test
// analog of a sequence of ArchiveChronicleDay calls
// without actually running the state tool (which would
// require a full GM wiring). Days not in the `days`
// slice are skipped (so the test can simulate a gap in
// the calendar — e.g. 1..29, 31..60 with day 30
// missing).
func writeChronicleDays(t *testing.T, fs *storage.FileStore, world string, days map[int]string) {
	t.Helper()
	c := chronicle.Chronicle{Periods: []chronicle.Period{}, Days: map[int]string{}}
	for d, txt := range days {
		c.AppendDay(d, txt)
	}
	body, err := c.Save()
	require.NoError(t, err)
	require.NoError(t, fs.WriteRawAtomic(fs.WorldChronicle(world), body))
}

// TestChronicleCompressWindow_Basic30Days writes days
// 1..30, calls the compression hook with day=30 which
// must collapse days 1..30 into a single Period.
func TestChronicleCompressWindow_Basic30Days(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	require.NoError(t, fs.EnsureDir("worlds/naruto"))
	days := make(map[int]string, 30)
	for i := 1; i <= 30; i++ {
		days[i] = "день " + strconv.Itoa(i)
	}
	writeChronicleDays(t, fs, "naruto", days)
	stub := &stubChronicleSummarizer{returnedMemory: "выжимка 30 дней"}
	mem := newMemory(fs, zerolog.Nop(), nil, nil, stub, nil)
	err := mem.chronicleCompressAfterArchive(context.Background(), "naruto", 30)
	require.NoError(t, err)
	assert.Equal(t, 1, stub.calls)
	assert.Equal(t, 1, stub.lastFrom)
	assert.Equal(t, 30, stub.lastTo)
	// File should now contain the Period with the
	// stub's memory text. The raw day entries for
	// 1..30 must be gone.
	body, _ := fs.ReadRaw(fs.WorldChronicle("naruto"))
	assert.Contains(t, body, "from: 1")
	assert.Contains(t, body, "to: 30")
	assert.Contains(t, body, "memory: выжимка 30 дней")
	// The days map should be empty (or at least not
	// contain the in-window days).
	c, err := chronicle.Load(body)
	require.NoError(t, err)
	for i := 1; i <= 30; i++ {
		_, present := c.Days[i]
		assert.False(t, present, "day %d should be removed", i)
	}
	assert.Equal(t, 1, len(c.Periods))
}

// TestChronicleCompressWindow_Ladder: archive up to day
// 60 → must produce TWO Period entries, one per window.
func TestChronicleCompressWindow_Ladder(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	require.NoError(t, fs.EnsureDir("worlds/naruto"))
	days := make(map[int]string, 60)
	for i := 1; i <= 60; i++ {
		days[i] = "день " + strconv.Itoa(i)
	}
	writeChronicleDays(t, fs, "naruto", days)
	stub := &stubChronicleSummarizer{returnedMemory: "выжимка"}
	mem := newMemory(fs, zerolog.Nop(), nil, nil, stub, nil)
	err := mem.chronicleCompressAfterArchive(context.Background(), "naruto", 60)
	require.NoError(t, err)
	assert.Equal(t, 2, stub.calls, "two windows → two LLM calls")
	c, err := chronicle.Load(mustRead(t, fs, "naruto"))
	require.NoError(t, err)
	require.Equal(t, 2, len(c.Periods))
	assert.Equal(t, 1, c.Periods[0].From)
	assert.Equal(t, 30, c.Periods[0].To)
	assert.Equal(t, 31, c.Periods[1].From)
	assert.Equal(t, 60, c.Periods[1].To)
}

// TestChronicleCompressWindow_Timeskip simulates a jump
// from day 10 to day 90 (e.g. the player left the world
// and returned). The file already has days 1..10; the
// hook is called as if day 90 was just archived. Three
// windows must be compressed: 1..30, 31..60, 61..90.
func TestChronicleCompressWindow_Timeskip(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	require.NoError(t, fs.EnsureDir("worlds/naruto"))
	days := make(map[int]string, 90)
	for i := 1; i <= 90; i++ {
		days[i] = "день " + strconv.Itoa(i)
	}
	writeChronicleDays(t, fs, "naruto", days)
	stub := &stubChronicleSummarizer{returnedMemory: "окно"}
	mem := newMemory(fs, zerolog.Nop(), nil, nil, stub, nil)
	err := mem.chronicleCompressAfterArchive(context.Background(), "naruto", 90)
	require.NoError(t, err)
	assert.Equal(t, 3, stub.calls, "three windows → three LLM calls")
	c, err := chronicle.Load(mustRead(t, fs, "naruto"))
	require.NoError(t, err)
	require.Equal(t, 3, len(c.Periods))
	assert.Equal(t, 1, c.Periods[0].From)
	assert.Equal(t, 30, c.Periods[0].To)
	assert.Equal(t, 31, c.Periods[1].From)
	assert.Equal(t, 60, c.Periods[1].To)
	assert.Equal(t, 61, c.Periods[2].From)
	assert.Equal(t, 90, c.Periods[2].To)
	assert.Equal(t, 0, len(c.Days), "all 90 days should be inside Periods")
}

// TestChronicleCompressWindow_NoSummarizer: nil summarizer
// must log a warning and leave the file untouched.
func TestChronicleCompressWindow_NoSummarizer(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	require.NoError(t, fs.EnsureDir("worlds/naruto"))
	days := make(map[int]string, 30)
	for i := 1; i <= 30; i++ {
		days[i] = "день"
	}
	writeChronicleDays(t, fs, "naruto", days)
	before, _ := fs.ReadRaw(fs.WorldChronicle("naruto"))
	mem := newMemory(fs, zerolog.Nop(), nil, nil, nil, nil)
	err := mem.chronicleCompressAfterArchive(context.Background(), "naruto", 30)
	require.NoError(t, err)
	after, _ := fs.ReadRaw(fs.WorldChronicle("naruto"))
	assert.Equal(t, before, after, "no summarizer → no write")
}

// TestChronicleCompressWindow_BadOutput: model returns
// empty body (the only kind of "bad" output the new
// chronicle API rejects — no bad-prefix concept
// anymore, the Memory field is free-form). The window
// must be left untouched.
func TestChronicleCompressWindow_BadOutput(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	require.NoError(t, fs.EnsureDir("worlds/naruto"))
	days := make(map[int]string, 30)
	for i := 1; i <= 30; i++ {
		days[i] = "день"
	}
	writeChronicleDays(t, fs, "naruto", days)
	before, _ := fs.ReadRaw(fs.WorldChronicle("naruto"))
	stub := &stubChronicleSummarizer{skipOutput: true}
	mem := newMemory(fs, zerolog.Nop(), nil, nil, stub, nil)
	err := mem.chronicleCompressAfterArchive(context.Background(), "naruto", 30)
	require.NoError(t, err)
	after, _ := fs.ReadRaw(fs.WorldChronicle("naruto"))
	assert.Equal(t, before, after, "empty output → no write")
}

// TestChronicleCompressWindow_TooThin: only 3 days in a
// 30-day window. Must skip the LLM call entirely.
func TestChronicleCompressWindow_TooThin(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	require.NoError(t, fs.EnsureDir("worlds/naruto"))
	days := map[int]string{1: "a", 5: "b", 10: "c"}
	writeChronicleDays(t, fs, "naruto", days)
	stub := &stubChronicleSummarizer{returnedMemory: "should not be called"}
	mem := newMemory(fs, zerolog.Nop(), nil, nil, stub, nil)
	err := mem.chronicleCompressAfterArchive(context.Background(), "naruto", 10)
	require.NoError(t, err)
	assert.Equal(t, 0, stub.calls, "window too thin (3 days out of 30) — no LLM call")
}

// TestChronicleCompressWindow_WithGap: days 1..29 and
// 31..60 are present, day 30 is missing. The window
// 1..30 must still be compressed (29 days is plenty),
// and the model receives the actual present-day list
// (start=1, end=29 in actualStart/actualEnd).
func TestChronicleCompressWindow_WithGap(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	require.NoError(t, fs.EnsureDir("worlds/naruto"))
	days := make(map[int]string, 59)
	for i := 1; i <= 29; i++ {
		days[i] = "день " + strconv.Itoa(i)
	}
	for i := 31; i <= 60; i++ {
		days[i] = "день " + strconv.Itoa(i)
	}
	writeChronicleDays(t, fs, "naruto", days)
	stub := &stubChronicleSummarizer{returnedMemory: "окно с дыркой"}
	mem := newMemory(fs, zerolog.Nop(), nil, nil, stub, nil)
	err := mem.chronicleCompressAfterArchive(context.Background(), "naruto", 60)
	require.NoError(t, err)
	assert.Equal(t, 2, stub.calls, "two windows → two LLM calls")
	// First window is days 1..29 (gap at 30).
	assert.Equal(t, 1, stub.firstFrom)
	assert.Equal(t, 29, stub.firstTo, "window 1..30 collapses to 1..29 because day 30 is absent")
	// Second window is days 31..60 (no gap).
	assert.Equal(t, 31, stub.lastFrom)
	assert.Equal(t, 60, stub.lastTo)
	c, err := chronicle.Load(mustRead(t, fs, "naruto"))
	require.NoError(t, err)
	assert.Equal(t, 2, len(c.Periods))
}

// mustRead is a small helper for tests that want to
// fail on read errors. chronicle.Load + ReadRaw
// returns err==nil for missing files (the FileStore
// returns ""), so we use require.NoError for parse
// failures and a t.Fatalf for I/O errors.
func mustRead(t *testing.T, fs *storage.FileStore, world string) string {
	t.Helper()
	body, err := fs.ReadRaw(fs.WorldChronicle(world))
	require.NoError(t, err)
	return body
}

// TestChronicleCompressWindow_KeepsEarlierSummaries: if
// the chronicle already has a Period for days 1..30
// and we archive day 60, only the days 31..60 window
// should be compressed. The earlier Period stays
// untouched.
func TestChronicleCompressWindow_KeepsEarlierSummaries(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	require.NoError(t, fs.EnsureDir("worlds/naruto"))
	// Build a chronicle with one Period (1..30) plus raw
	// days 31..60.
	c := chronicle.Chronicle{Periods: []chronicle.Period{{From: 1, To: 30, Memory: "старая сводка 1"}}, Days: map[int]string{}}
	for i := 31; i <= 60; i++ {
		c.AppendDay(i, "день "+strconv.Itoa(i))
	}
	body, err := c.Save()
	require.NoError(t, err)
	require.NoError(t, fs.WriteRawAtomic(fs.WorldChronicle("naruto"), body))
	stub := &stubChronicleSummarizer{returnedMemory: "новая сводка 2"}
	mem := newMemory(fs, zerolog.Nop(), nil, nil, stub, nil)
	err = mem.chronicleCompressAfterArchive(context.Background(), "naruto", 60)
	require.NoError(t, err)
	assert.Equal(t, 1, stub.calls, "only one window left to compress")
	assert.Equal(t, 31, stub.lastFrom)
	assert.Equal(t, 60, stub.lastTo)
	out, err := chronicle.Load(mustRead(t, fs, "naruto"))
	require.NoError(t, err)
	require.Equal(t, 2, len(out.Periods), "earlier Period + new Period")
	assert.Equal(t, 1, out.Periods[0].From)
	assert.Equal(t, 30, out.Periods[0].To)
	assert.Equal(t, "старая сводка 1", out.Periods[0].Memory,
		"earlier summary must NOT be touched")
	assert.Equal(t, 31, out.Periods[1].From)
	assert.Equal(t, 60, out.Periods[1].To)
	assert.Equal(t, "новая сводка 2", out.Periods[1].Memory)
}

// TestStateArchiveDay_TriggersCompression: end-to-end
// through State.ArchiveChronicleDay, the
// ChronicleCompressWindow hook must fire on a closing
// day. Day 30 closes the 1..30 window — the just-
// archived day's text "итог" is included in the raw
// days the LLM sees. The result is a single Period
// covering the whole window.
func TestStateArchiveDay_TriggersCompression(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	require.NoError(t, fs.EnsureDir("worlds/naruto"))
	// 29 days, day 30 will be added by ArchiveChronicleDay.
	days := make(map[int]string, 29)
	for i := 1; i <= 29; i++ {
		days[i] = "день"
	}
	writeChronicleDays(t, fs, "naruto", days)
	stub := &stubChronicleSummarizer{returnedMemory: "сводка"}
	mem := newMemory(fs, zerolog.Nop(), nil, nil, stub, nil)
	st := newState(fs, zerolog.Nop(), slowlog.Discard())
	st.SetChronicleCompress(mem.chronicleCompressAfterArchive)
	require.NoError(t, st.ArchiveChronicleDay(context.Background(), "naruto", 30, "итог"))
	assert.Equal(t, 1, stub.calls)
	assert.Equal(t, 30, stub.lastTo)
	out, err := chronicle.Load(mustRead(t, fs, "naruto"))
	require.NoError(t, err)
	require.Equal(t, 1, len(out.Periods))
	assert.Equal(t, 1, out.Periods[0].From)
	assert.Equal(t, 30, out.Periods[0].To)
	assert.Equal(t, "сводка", out.Periods[0].Memory,
		"the 30-day window was compressed into a single Period")
	// Day 30 must NOT be in the raw days map — it was
	// absorbed into the Period.
	_, present := out.Days[30]
	assert.False(t, present, "day 30 is part of the compressed Period")
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
	writeChronicleDays(t, fs, "naruto", days)
	stub := &stubChronicleSummarizer{returnedMemory: "no"}
	mem := newMemory(fs, zerolog.Nop(), nil, nil, stub, nil)
	st := newState(fs, zerolog.Nop(), slowlog.Discard())
	st.SetChronicleCompress(mem.chronicleCompressAfterArchive)
	require.NoError(t, st.ArchiveChronicleDay(context.Background(), "naruto", 25, "x"))
	assert.Equal(t, 0, stub.calls, "day 25 is not a window boundary")
}
