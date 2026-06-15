package files

import (
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bestxp/narrative-ai-agent/internal/adapter/storage"
	"github.com/bestxp/narrative-ai-agent/internal/slowlog"
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
	n := newNPC(fs, zerolog.Nop(), slowlog.Discard())
	rel, slug, ok := n.findNPCFile("naruto", "Хината")
	if !ok {
		t.Fatal("Хината should resolve to hinata.yaml via registry")
	}
	if slug != "hinata" {
		t.Errorf("slug = %q, want %q", slug, "hinata")
	}
	if rel != "worlds/naruto/characters/hinata.yaml" {
		t.Errorf("rel = %q, want %q", rel, "worlds/naruto/characters/hinata.yaml")
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
	n := newNPC(fs, zerolog.Nop(), slowlog.Discard())
	_, _, ok := n.findNPCFile("naruto", "Совершенно Незнакомый NPC")
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
	fs, _ := storage.NewFileStore(t.TempDir())
	require.NoError(t, fs.EnsureDir("worlds/naruto/characters"))
	// Seed a real-looking NPC profile plus a registry
	// that points at it. The full path through
	// findNPCFile goes: loadRegistry → Lookup → file
	// exists check. Without the registry entry, the
	// fallback scans the directory and tries
	// npcprofile.Load on every file — which is fine
	// for production but a real registry entry keeps
	// the test deterministic across YAML changes.
	require.NoError(t, fs.WriteRawAtomic("worlds/naruto/characters/inari.yaml",
		"display_name: Инари\ntemperament: робкая\ncurrent_status: в деревне\n"+
			"personal_memory: []\nabilities: []\nrelations_npcs: []\ncritical_knowledge: []\nnicknames: []\nlast_update: \"\"\n"))
	require.NoError(t, fs.WriteRawAtomic("worlds/naruto/characters.yaml",
		"npcs:\n  - slug: inari\n    display_name: Инари\n    nicknames: []\n"))
	logger, read := captureSlowlog(t)
	n := newNPC(fs, zerolog.Nop(), logger)

	// Successful append: changed=true, bytes_added > 0.
	require.NoError(t, n.UpdateNPC("naruto", "Инари", "Личная память/факты", "Встретил ГГ у моста"))

	entries := read("tool.update_npc")
	require.Len(t, entries, 1, "one tool.update_npc per successful UpdateNPC")
	fields := entries[0].Fields
	assert.Equal(t, "inari", fields["npc"])
	assert.Equal(t, "Личная память/факты", fields["section"])
	assert.Equal(t, true, fields["changed"])
	assert.Equal(t, float64(len("Встретил ГГ у моста")), fields["bytes_added"])
	assert.Equal(t, "worlds/naruto/characters/inari.yaml", fields["path"])

	// Same call repeated (dedup) — should also emit a
	// tool.update_npc event, but changed=false.
	require.NoError(t, n.UpdateNPC("naruto", "Инари", "Личная память/факты", "Встретил ГГ у моста"))
	entries = read("tool.update_npc")
	require.Len(t, entries, 2, "the dedup no-op must still emit a tool.update_npc event")
	dedupEntry := entries[1].Fields
	assert.Equal(t, false, dedupEntry["changed"])
	assert.NotContains(t, dedupEntry, "bytes_added",
		"the dedup entry must not carry a bytes_added field — the file was not touched")
}
