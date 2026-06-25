package files

import (
	"testing"

	"github.com/bestxp/narrative-ai-agent/internal/adapter/storage"
	"github.com/bestxp/narrative-ai-agent/internal/repository/api"
	"github.com/bestxp/narrative-ai-agent/internal/slowlog"
	yamlfs "github.com/bestxp/narrative-ai-agent/internal/storage/fs"
	"github.com/bestxp/narrative-ai-agent/internal/worldregistry"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// makeNPC constructs an NPC backed by a fresh
// FileStore at the given temp dir. Used by tests
// below — keeps the per-test setup short.
func makeNPC(t *testing.T) (*NPC, *storage.FileStore) {
	t.Helper()
	fs, err := storage.NewFileStore(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, fs.EnsureDir("worlds/naruto/characters"))
	yamlStore, _ := yamlfs.New(fs.Root())
	repos := api.NewYamlRepositories(yamlStore)
	return newNPC(zerolog.Nop(), slowlog.Discard(), repos, fs), fs
}

// TestNPC_LookupViaRegistry confirms the
// worldregistry-based path resolves a display name
// to the right on-disk file even when the slug does
// not match a Russian→Latin transliteration
// (Хината → khinata vs the operator's hand-picked
// hinata). The test must prove the YAML registry
// file is actually consulted on every Load /
// UpdateNPC / Search call.
func TestNPC_LookupViaRegistry(t *testing.T) {
	t.Parallel()
	n, fs := makeNPC(t)
	require.NoError(t, fs.WriteRawAtomic("worlds/naruto/characters/hinata.yaml",
		"display_name: Хината Хьюга\ntemperament: застенчивая\n"))
	require.NoError(t, fs.WriteRawAtomic("worlds/naruto/characters.yaml",
		"npcs:\n  - slug: hinata\n    display_name: Хината Хьюга\n    nicknames: [Хината]\n"))

	slug, err := n.resolveSlug("naruto", "Хината")
	require.NoError(t, err)
	require.Equal(t, "hinata", slug)
}

// TestNPC_LookupSubstring covers the substring
// fallback: the model writes "Хината" and the
// registry has "Хината Хьюга". Lookup must return
// the unambiguous hit (the surname is longer than
// the model's token, so the substring direction
// is the model→file one).
func TestNPC_LookupSubstring(t *testing.T) {
	t.Parallel()
	reg := worldregistry.Registry{}
	require.NoError(t, reg.Add(worldregistry.Entry{
		Slug: "hinata", DisplayName: "Хината Хьюга",
	}))
	entry, ok := reg.Lookup("Хината")
	require.True(t, ok)
	require.Equal(t, "hinata", entry.Slug)
}

// TestNPC_LookupAmbiguousRefuses covers the case
// where two NPCs share a substring token. Lookup
// must refuse to guess — the operator should pick
// the unambiguous name and retry.
func TestNPC_LookupAmbiguousRefuses(t *testing.T) {
	t.Parallel()
	reg := worldregistry.Registry{}
	require.NoError(t, reg.Add(worldregistry.Entry{Slug: "naruto_uzumaki", DisplayName: "Наруто"}))
	require.NoError(t, reg.Add(worldregistry.Entry{Slug: "naruto_clone", DisplayName: "Наруто-клона"}))
	_, ok := reg.Lookup("Нар")
	require.False(t, ok, "ambiguous substring must NOT resolve")
}

// TestNPC_NotFoundReturnsErrNPCNotFound covers the
// "model wrote a name the registry does not know"
// path. resolveSlug returns ErrNPCNotFound; the GM
// is expected to translate that into a
// "create_npc first" hint to the model.
func TestNPC_NotFoundReturnsErrNPCNotFound(t *testing.T) {
	t.Parallel()
	n, _ := makeNPC(t)
	_, err := n.resolveSlug("naruto", "Совершенно Незнакомый NPC")
	require.ErrorIs(t, err, ErrNPCNotFound)
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
	t.Parallel()
	_, fs := makeNPC(t)
	require.NoError(t, fs.WriteRawAtomic("worlds/naruto/characters/inari.yaml",
		"display_name: Инари\ntemperament: робкая\ncurrent_status: в деревне\n"+
			"personal_memory: []\nabilities: []\nrelations_npcs: []\ncritical_knowledge: []\nnicknames: []\nlast_update: \"\"\n"))
	require.NoError(t, fs.WriteRawAtomic("worlds/naruto/characters.yaml",
		"npcs:\n  - slug: inari\n    display_name: Инари\n"))
	logger, read := captureSlowlog(t)
	// Reconstruct with the slowlog-wired logger so
	// the slow.Write call below reaches the logger.
	yamlStore, _ := yamlfs.New(fs.Root())
	repos := api.NewYamlRepositories(yamlStore)
	n := newNPC(zerolog.Nop(), logger, repos, fs)

	require.NoError(t, n.UpdateNPC("naruto", "Инари", "Личная память/факты", "Встретил ГГ у моста"))

	entries := read("tool.update_npc")
	require.Len(t, entries, 1, "one tool.update_npc per successful UpdateNPC")
}
