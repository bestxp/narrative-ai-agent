package worldregistry

import (
	"strings"
	"testing"
)

type fakeFS struct {
	files map[string]string
}

func (f *fakeFS) ReadRaw(rel string) (string, error) {
	v, ok := f.files[rel]
	if !ok {
		return "", &fsErr{rel: rel}
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

type fsErr struct{ rel string }

func (e *fsErr) Error() string { return "fs: not found: " + e.rel }

// TestLoadFromYAML covers the canonical case: a
// world already has characters.yaml and the
// registry loads it as-is, no bootstrap.
func TestLoadFromYAML(t *testing.T) {
	fs := &fakeFS{files: map[string]string{
		"worlds/naruto/characters.yaml": `npcs:
  - slug: hinata
    display_name: Хината Хьюга
  - slug: naruto_uzumaki
    display_name: Наруто Узумаки
    nicknames: [Демон-лис, Блондинка]
`,
	}}
	r, err := Load(fs, "naruto")
	if err != nil {
		t.Fatal(err)
	}
	if len(r.entries) != 2 {
		t.Fatalf("entries=%d, want 2", len(r.entries))
	}
	if r.entries[0].Slug != "hinata" {
		t.Errorf("first slug = %q, want hinata", r.entries[0].Slug)
	}
}

// TestBootstrapFromLegacyMarkdown covers the
// migration path: an operator who has been
// running on characters.md gets a characters.yaml
// written on the first load. We do not delete the
// markdown file — the operator may still want it
// for human reference.
func TestBootstrapFromLegacyMarkdown(t *testing.T) {
	fs := &fakeFS{files: map[string]string{
		"worlds/naruto/characters.md": `# NPC: Наруто
| Имя | Файл | Прозвища |
|-----|------|----------|
| Хината Хьюга | characters/hinata |  |
| Наруто Узумаки | characters/naruto_uzumaki | Демон-лис, Блондинка |
| Ирука-сенсей | characters/iruka | сенсей |
`,
	}}
	r, err := Load(fs, "naruto")
	if err != nil {
		t.Fatal(err)
	}
	if len(r.entries) != 3 {
		t.Fatalf("entries=%d, want 3", len(r.entries))
	}
	// The yaml file should have been written.
	if _, ok := fs.files["worlds/naruto/characters.yaml"]; !ok {
		t.Fatal("expected characters.yaml to be created on first load")
	}
	// Substring lookup (model writes "Хината",
	// file has "Хината Хьюга") must resolve.
	entry, ok := r.Lookup("Хината")
	if !ok {
		t.Fatal("substring lookup must hit")
	}
	if entry.Slug != "hinata" {
		t.Errorf("slug = %q, want hinata", entry.Slug)
	}
	// Nickname lookup.
	entry, ok = r.Lookup("сенсей")
	if !ok {
		t.Fatal("nickname lookup must hit")
	}
	if entry.Slug != "iruka" {
		t.Errorf("nickname slug = %q, want iruka", entry.Slug)
	}
}

// TestLookupSlug covers the case where the model
// occasionally writes the file name
// ("naruto_uzumaki") instead of the display_name.
// The registry must accept both.
func TestLookupSlug(t *testing.T) {
	r := &Registry{}
	_ = r.Add(Entry{Slug: "naruto_uzumaki", DisplayName: "Наруто Узумаки"})
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
	r := &Registry{}
	_ = r.Add(Entry{Slug: "hinata", DisplayName: "Хината"})
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
	r := &Registry{}
	_ = r.Add(Entry{Slug: "hinata", DisplayName: "Хината"})
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
	r := &Registry{}
	if err := r.Add(Entry{Slug: "hinata", DisplayName: "Хината"}); err != nil {
		t.Fatal(err)
	}
	err := r.Add(Entry{Slug: "hinata", DisplayName: "Хината Хьюга"})
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
	r := &Registry{}
	_ = r.Add(Entry{Slug: "zzz", DisplayName: "Z"})
	_ = r.Add(Entry{Slug: "aaa", DisplayName: "A"})
	_ = r.Add(Entry{Slug: "mmm", DisplayName: "M"})
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
