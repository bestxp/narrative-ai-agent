package worldregistry_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/bestxp/narrative-ai-agent/internal/worldregistry"
)

type fakeFS struct {
	files map[string]string
}

func (f *fakeFS) ReadRaw(rel string) (string, error) {
	v, ok := f.files[rel]
	if !ok {
		return "", &fsError{rel: rel}
	}

	return v, nil
}

func (f *fakeFS) WriteRawAtomic(rel, body string) error {
	if f.files == nil {
		f.files = map[string]string{}
	}

	f.files[rel] = body

	return nil
}

func (f *fakeFS) Exists(rel string) bool {
	_, ok := f.files[rel]

	return ok
}

type fsError struct{ rel string }

func (e *fsError) Error() string { return "fs: not found: " + e.rel }

// TestLoadFromYAML covers the canonical case: a
// world already has characters.yaml and the
// registry loads it as-is. There is no characters.md
// fallback — the canonical roster is characters.yaml
// and nothing else.
func TestLoadFromYAML(t *testing.T) {
	t.Parallel()

	fs := &fakeFS{files: map[string]string{
		"worlds/naruto/characters.yaml": `npcs:
  - slug: hinata
    display_name: Хината Хьюга
  - slug: naruto_uzumaki
    display_name: Наруто Узумаки
    nicknames: [Демон-лис, Блондинка]
`,
	}}

	r, err := worldregistry.Load(fs, "naruto")
	if err != nil {
		t.Fatal(err)
	}

	entries := r.All()
	if len(entries) != 2 {
		t.Fatalf("entries=%d, want 2", len(entries))
	}

	if entries[0].Slug != "hinata" {
		t.Errorf("first slug = %q, want hinata", entries[0].Slug)
	}
}

// TestLoadEmpty covers a fresh world with no
// characters.yaml yet: Load returns an empty
// registry without an error. The first create_npc
// call will seed it.
func TestLoadEmpty(t *testing.T) {
	t.Parallel()

	fs := &fakeFS{files: map[string]string{}}

	r, err := worldregistry.Load(fs, "naruto")
	if !errors.Is(err, worldregistry.ErrEmpty) {
		t.Fatalf("err = %v, want ErrEmpty", err)
	}

	if r == nil {
		t.Fatal("registry is nil despite ErrEmpty contract")
	}

	if got := len(r.All()); got != 0 {
		t.Fatalf("entries=%d, want 0", got)
	}
}

// TestNoMarkdownFallback ensures the loader does
// NOT silently pick up a characters.md file: if the
// canonical YAML is missing the result is an empty
// registry. The legacy markdown path was removed
// because it produced duplicate-NPC cases where one
// registry listed a character that the other did
// not.
func TestNoMarkdownFallback(t *testing.T) {
	t.Parallel()

	fs := &fakeFS{files: map[string]string{
		"worlds/naruto/characters.md": `# NPC: naruto
| Имя | Файл | Прозвища |
|-----|------|----------|
| Хината Хьюга | hinata |  |
`,
	}}

	r, err := worldregistry.Load(fs, "naruto")
	if !errors.Is(err, worldregistry.ErrEmpty) {
		t.Fatalf("err = %v, want ErrEmpty", err)
	}

	if r == nil {
		t.Fatal("registry is nil despite ErrEmpty contract")
	}

	if got := len(r.All()); got != 0 {
		t.Fatalf("entries=%d, want 0 (markdown must be ignored)", got)
	}

	if _, ok := fs.files["worlds/naruto/characters.yaml"]; ok {
		t.Fatal("characters.yaml must NOT be created from a characters.md side-channel")
	}
}

// TestLookupSlug covers the case where the model
// occasionally writes the file name
// ("naruto_uzumaki") instead of the display_name.
// The registry must accept both.
func TestLookupSlug(t *testing.T) {
	t.Parallel()

	r := &worldregistry.Registry{}
	_ = r.Add(worldregistry.Entry{Slug: "naruto_uzumaki", DisplayName: "Наруто Узумаки"})

	e, ok := r.Lookup("naruto_uzumaki")
	if !ok {
		t.Fatal("slug lookup must hit")
	}

	if e.DisplayName != "Наруто Узумаки" {
		t.Errorf("display_name = %q, want Наруто Узумаки", e.DisplayName)
	}
}

// TestLookupEmpty covers the degenerate input
// paths. An empty string and a whitespace-only
// string are both "not found" — we do not want a
// silent miss-and-create on a malformed
// directive.
func TestLookupEmpty(t *testing.T) {
	t.Parallel()

	r := &worldregistry.Registry{}

	_ = r.Add(worldregistry.Entry{Slug: "hinata", DisplayName: "Хината"})
	for _, q := range []string{"", "   ", "\t"} {
		if _, ok := r.Lookup(q); ok {
			t.Errorf("query %q must not match", q)
		}
	}
}

// TestLookupTrimsWhitespace covers the
// "Хината " (trailing space) input. The trim
// happens before the lowercase, so a stray
// trailing newline from the model still resolves.
func TestLookupTrimsWhitespace(t *testing.T) {
	t.Parallel()

	r := &worldregistry.Registry{}

	_ = r.Add(worldregistry.Entry{Slug: "hinata", DisplayName: "Хината"})
	if _, ok := r.Lookup("  Хината  "); !ok {
		t.Fatal("trailing whitespace must not break lookup")
	}
}

// TestAddDuplicateSlug is a safety net: the
// operator hand-edits characters.yaml and
// accidentally puts the same slug twice. Add
// refuses rather than silently overwriting — the
// caller (Create) logs and continues.
func TestAddDuplicateSlug(t *testing.T) {
	t.Parallel()

	r := &worldregistry.Registry{}
	if err := r.Add(worldregistry.Entry{Slug: "hinata", DisplayName: "Хината"}); err != nil {
		t.Fatal(err)
	}

	err := r.Add(worldregistry.Entry{Slug: "hinata", DisplayName: "Хината Хьюга"})
	if err == nil {
		t.Fatal("duplicate slug must be rejected")
	}

	if !strings.Contains(err.Error(), "hinata") {
		t.Errorf("error must name the slug, got: %v", err)
	}
}

// TestSaveSortsBySlug: the YAML output is sorted so
// the operator's diff in git is minimal when
// characters.yaml is touched. We verify the sort
// order explicitly.
func TestSaveSortsBySlug(t *testing.T) {
	t.Parallel()

	r := &worldregistry.Registry{}
	_ = r.Add(worldregistry.Entry{Slug: "zzz", DisplayName: "Z"})
	_ = r.Add(worldregistry.Entry{Slug: "aaa", DisplayName: "A"})
	_ = r.Add(worldregistry.Entry{Slug: "mmm", DisplayName: "M"})

	out, err := r.Save()
	if err != nil {
		t.Fatal(err)
	}

	idxA := strings.Index(out, "aaa")
	idxM := strings.Index(out, "mmm")

	idxZ := strings.Index(out, "zzz")
	if idxA < 0 || idxM < 0 || idxZ < 0 {
		t.Fatalf("slugs missing from output:\n%s", out)
	}

	if idxA >= idxM || idxM >= idxZ {
		t.Fatalf("not sorted: aaa=%d mmm=%d zzz=%d", idxA, idxM, idxZ)
	}
}
