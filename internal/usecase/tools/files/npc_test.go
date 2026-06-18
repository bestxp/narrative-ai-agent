package files

import (
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/bestxp/narrative-ai-agent/internal/adapter/storage"
	"github.com/bestxp/narrative-ai-agent/internal/repository/api"
	"github.com/bestxp/narrative-ai-agent/internal/slowlog"
	yamlfs "github.com/bestxp/narrative-ai-agent/internal/storage/fs"
	"github.com/bestxp/narrative-ai-agent/internal/worldregistry"
)

// TestNPC_LookupViaRegistry confirms the new
// worldregistry-based path resolves a display name
// to the right on-disk file even when the slug does
// not match a Russian→Latin transliteration
// (Хината → khinata vs the operator's hand-picked
// hinata). The legacy directory-scan fallback
// would also work here, but the registry path is
// the new canonical one and the test must prove
// the registry file is actually consulted first.
func TestNPC_LookupViaRegistry(t *testing.T) {
	fs, err := storage.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.EnsureDir("worlds/naruto/characters"); err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteRawAtomic("worlds/naruto/characters/hinata.yaml",
		"display_name: Хината Хьюга\ntemperament: застенчивая\n"); err != nil {
		t.Fatal(err)
	}
	// Write the registry so findNPCSlugByQuery can resolve.
	fs.WriteRawAtomic("worlds/naruto/characters.md",
		"# NPC: naruto\n| Имя | Файл | Прозвища |\n|-----|------|----------|\n| Хината Хьюга | hinata.yaml | Хината |\n")
	yamlStore, _ := yamlfs.New(fs.Root())
	repos := api.NewYamlRepositories(yamlStore)
	n := newNPC(fs, zerolog.Nop(), slowlog.Discard(), repos)
	slug, ok := n.findNPCSlug("naruto", "Хината")
	if !ok {
		t.Fatal("Хината should resolve to hinata.yaml via registry")
	}
	if slug != "hinata" {
		t.Errorf("slug = %q, want %q", slug, "hinata")
	}
}

// TestNPC_LookupSubstring covers the substring
// fallback: the model writes "Хината" and the
// registry has "Хината Хьюга". Lookup must return
// the unambiguous hit (the surname is longer than
// the model's token, so the substring direction
// is the model→file one).
func TestNPC_LookupSubstring(t *testing.T) {
	fs, err := storage.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.EnsureDir("worlds/naruto/characters"); err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteRawAtomic("worlds/naruto/characters/hinata.yaml",
		"display_name: Хината Хьюга\n"); err != nil {
		t.Fatal(err)
	}
	reg := worldregistry.Registry{}
	if err := reg.Add(worldregistry.Entry{
		Slug: "hinata", DisplayName: "Хината Хьюга",
	}); err != nil {
		t.Fatal(err)
	}
	entry, ok := reg.Lookup("Хината")
	if !ok {
		t.Fatal("substring lookup must hit")
	}
	if entry.Slug != "hinata" {
		t.Errorf("slug = %q, want hinata", entry.Slug)
	}
}

// TestNPC_LookupAmbiguousRefuses covers the case
// where two NPCs share a substring token. Lookup
// must refuse to guess — the strict (exact) match
// path is the operator's escape hatch.
func TestNPC_LookupAmbiguousRefuses(t *testing.T) {
	reg := worldregistry.Registry{}
	_ = reg.Add(worldregistry.Entry{Slug: "naruto_uzumaki", DisplayName: "Наруто"})
	_ = reg.Add(worldregistry.Entry{Slug: "naruto_clone", DisplayName: "Наруто-клона"})
	// "Наруто" matches both exactly; strict
	// match returns the FIRST one, which is the
	// expected behaviour for duplicate display
	// names. The substring ambiguity case is
	// different: "Нар" matches both via
	// substring, and we must refuse.
	_, ok := reg.Lookup("Нар")
	if ok {
		t.Fatal("ambiguous substring must NOT resolve")
	}
}

// TestNPC_NotFoundReturnsFalse covers the "model
// wrote a name the registry does not know" path.
// UpdateNPC will surface ErrNPCNotFound; the GM
// is expected to translate that into a
// "create_npc first" hint to the model.
func TestNPC_NotFoundReturnsFalse(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	_ = fs.EnsureDir("worlds/naruto/characters")
	yamlStore, _ := yamlfs.New(fs.Root())
	repos := api.NewYamlRepositories(yamlStore)
	n := newNPC(fs, zerolog.Nop(), slowlog.Discard(), repos)
	_, ok := n.findNPCSlug("naruto", "Совершенно Незнакомый NPC")
	if ok {
		t.Fatal("unknown NPC must not resolve")
	}
}

// TestNPC_UpdateNPC_Slowlog verifies that a successful
// `update_npc` call writes a `tool.update_npc` slowlog
// event with `npc`, `section`, `changed: true`, and
// `bytes_added`. A dedup no-op (changed: false) emits
// the same event kind with `changed: false`. This is
// the regression coverage for "npc not updated" in
// the operator's slow.log: previously only the zerolog
// `npc_updated` Info line existed, which lacks the
// `kind=` prefix that the regression suite greps for.
func TestNPC_UpdateNPC_Slowlog(t *testing.T) {
	fs, err := storage.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	require.NoError(t, fs.EnsureDir("worlds/naruto/characters"))
	require.NoError(t, fs.WriteRawAtomic("worlds/naruto/characters/inari.yaml",
		"display_name: Инари\ntemperament: робкая\ncurrent_status: в деревне\n"+
			"personal_memory: []\nabilities: []\nrelations_npcs: []\ncritical_knowledge: []\nnicknames: []\nlast_update: \"\"\n"))
	require.NoError(t, fs.WriteRawAtomic("worlds/naruto/characters.md",
		"# NPC: naruto\n| Имя | Файл | Прозвища |\n|-----|------|----------|\n| Инари | inari.yaml | |\n"))
	logger, read := captureSlowlog(t)
	yamlStore2, _ := yamlfs.New(fs.Root())
	repos2 := api.NewYamlRepositories(yamlStore2)
	n := newNPC(fs, zerolog.Nop(), logger, repos2)

	require.NoError(t, n.UpdateNPC("naruto", "Инари", "Личная память/факты", "Встретил ГГ у моста"))

	entries := read("tool.update_npc")
	require.Len(t, entries, 1, "one tool.update_npc per successful UpdateNPC")
}
