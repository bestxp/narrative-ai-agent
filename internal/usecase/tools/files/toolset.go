// Package files is the filesystem-backed implementation of
// the tools.Tool interface. It is the only "real" backend the
// production binary uses today; the package boundary keeps
// the adapter isolated so a future s3 / git / in-memory
// backend can drop in without touching gm.go or dispatcher.go.
//
// In the post-repository world (see research_repository_pattern.md)
// the Toolset no longer touches the storage layer directly.
// Every persistent operation goes through the repository
// bundle (*api.Repositories) — the file backend
// *YamlStorage lives behind the 5-operation Storage
// interface, and the YAML repositories wrap it into
// domain-shaped APIs.
package files

import (
	"context"
	"time"

	"github.com/rs/zerolog"

	"github.com/bestxp/narrative-ai-agent/internal/adapter/storage"
	"github.com/bestxp/narrative-ai-agent/internal/charprofile"
	"github.com/bestxp/narrative-ai-agent/internal/repository/api"
	"github.com/bestxp/narrative-ai-agent/internal/slowlog"
	"github.com/bestxp/narrative-ai-agent/internal/usecase/tools"
)

// Toolset is the repository-backed implementation of
// tools.Tool. It composes one struct per concern
// (state, memory, world, character, NPC, staging);
// each concern holds a reference to the shared
// repository bundle.
//
// Why each concern is its own struct (not one big
// monolith):
//
//   - cognitive load — 200-line files beat 1000-line
//     ones;
//   - testability — a test that needs only the NPC
//     concern holds a *files.NPC reference, not a
//     full Toolset;
//   - LLM-driven hooks (MaintainNPCs, MaintainLore,
//     ChronicleCompressWindow, MaintainCharacterMemory)
//     live on *Memory because they are cross-domain
//     side-effects of the per-file write path.
type Toolset struct {
	*State
	*Memory
	*World
	*Character
	*NPC
	*StageTool

	// repos is the bundle every concern reads from.
	// Held at the Toolset level so sub-structs can be
	// built without re-plumbing the dependency.
	repos *api.Repositories
}

// New constructs the repository-backed toolset.
//
// log is component-tagged per concern when needed (so a
// maintenance event and an NPC event get different
// "component" fields in zerolog). slow is the optional
// audit log; pass slowlog.Discard() in tests.
//
// summarizer is the LLM-driven NPC condensation hook used
// by MaintainNPCs. loreSummarizer is the LLM-driven
// lore.md compaction hook used by MaintainLore.
// chronicleSummarizer is the LLM-driven 30-day window
// compression hook used by ArchiveChronicleDay.
// characterMemorySummarizer is the LLM-driven memory.yaml
// defragmentation hook used by MaintainCharacterMemory
// (end-of-day pass). Pass nil to any of them to disable
// the LLM path — the file backend will then log a warning
// and skip.
//
// The NPC concern reads characters.yaml through the
// FileStore (via the worldregistry package), so the
// constructor takes fs alongside repos. fs is used only
// by NPC; other concerns continue to read through repos.
func New(fs *storage.FileStore, repos *api.Repositories, log zerolog.Logger, slow *slowlog.Logger, summarizer tools.NPCSummarizer, loreSummarizer tools.LoreSummarizer, chronicleSummarizer tools.ChronicleSummarizer, characterMemorySummarizer tools.CharacterMemorySummarizer) *Toolset {
	mem := newMemory(log, summarizer, loreSummarizer, chronicleSummarizer, characterMemorySummarizer, repos)
	st := newState(log, slow, repos)
	// Wire the post-ArchiveChronicleDay hook so the
	// state writer does not need a direct reference to
	// the memory struct. The hook is nil-safe — it logs
	// and skips when no summarizer is wired.
	st.SetChronicleCompress(mem.chronicleCompressAfterArchive)
	return &Toolset{
		State:     st,
		Memory:    mem,
		World:     newWorld(log, repos),
		Character: newCharacter(repos, log, slow),
		NPC:       newNPC(log, slow, repos, fs),
		StageTool: newStage(log, repos),
		repos:     repos,
	}
}

// SetWorldStateInvalidate lets main.go (or any wiring
// point that has access to the GM) install the cache
// invalidation callback. Called once at boot.
func (t *Toolset) SetWorldStateInvalidate(fn func(reason string)) {
	t.State.SetWorldStateInvalidate(fn)
	t.World.SetWorldStateInvalidate(fn)
}

// AsToolset returns a *tools.Tool view of this backend. The
// returned type is just the interface; Toolset itself
// already satisfies it, so callers that hold a *Toolset
// can pass it as tools.Tool without an explicit cast.
// This method exists for symmetry with future backends
// that may wrap the concrete struct in a different way.
func (t *Toolset) AsToolset() tools.Tool {
	return t
}

// Source identifies the backend. Pure cosmetic; included
// in slowlog events and /status output so the operator
// knows which data source is wired.
func (t *Toolset) Source() string { return "files" }

// Repositories returns the bundle the toolset delegates
// to. Exposed for tests that need to assert on the
// post-write state of a repository without reading
// from the filesystem directly.
func (t *Toolset) Repositories() *api.Repositories {
	return t.repos
}

// Compile-time check: *Toolset must satisfy tools.Tool. If
// a method is renamed or its signature drifts the build
// fails here, not in main.go far away from the cause.
var _ tools.Tool = (*Toolset)(nil)

// MaintainLore is a thin forwarder to the embedded
// *Memory. The interface declares MaintainLore(ctx, world)
// (with a context for the summarizer LLM call) so the
// per-request deadline applies; the GM and the
// /maintenance dispatcher path supply their own
// context. main.go is the only caller that does NOT
// supply a context — it does not call this method
// directly.
func (t *Toolset) MaintainLore(ctx context.Context, world string) (bool, error) {
	return t.Memory.MaintainLore(ctx, world)
}

// MaintainCharacterMemory is the end-of-day hook
// for the active character's memory.yaml. Forwarder
// to the embedded *Memory. The interface takes a
// context so the per-turn deadline applies; the
// /maintenance operator path supplies a longer one.
func (t *Toolset) MaintainCharacterMemory(ctx context.Context, world, character string) (bool, error) {
	return t.Memory.MaintainCharacterMemory(ctx, world, character)
}

// SearchNPC resolves a free-form query against the
// world's NPC registry and returns a compact
// description. The Tool interface returns a
// *tools.NPCSearchResult (decoupled from the file
// backend); this forwarder adapts the file package's
// *SearchResult to the interface type at the boundary.
func (t *Toolset) SearchNPC(world, query string) (*tools.NPCSearchResult, error) {
	defer func() {
		_ = recover()
	}()
	if t.NPC == nil {
		return nil, nil
	}
	res, e := t.Search(world, query)
	if e != nil {
		return nil, e
	}
	return &tools.NPCSearchResult{
		DisplayName:   res.DisplayName,
		Slug:          res.Slug,
		Temperament:   res.Temperament,
		CurrentStatus: res.CurrentStatus,
		Source:        res.Source,
	}, nil
}

// LoadLOD is the LOD-aware counterpart to Load. The
// file package renders at the requested detail;
// callers (loadActiveNPCs) apply the LOD policy.
func (t *Toolset) LoadLOD(world, npc string, lod tools.NPCLOD) (string, error) {
	return t.NPC.LoadLOD(world, npc, lod)
}

// EndScene forwards to state.EndScene. The interface
// returns *tools.EndSceneResult; the file package's
// *EndSceneResult has the same shape (KeptNPCs +
// PrunedNPCsLen), so the conversion is a field-copy.
func (t *Toolset) EndScene(world string, permanentParty []string) (*tools.EndSceneResult, error) {
	res, err := t.State.EndScene(world, permanentParty)
	if err != nil {
		return nil, err
	}
	return &tools.EndSceneResult{
		KeptNPCs:      res.KeptNPCs,
		PrunedNPCsLen: res.PrunedNPCsLen,
	}, nil
}

// --- character file forwarders (h5 refactor) ---
//
// The legacy single Append(file=...) dispatcher is
// gone. Each per-file Append* is a thin pass-through
// to the embedded *Character. The split mirrors the
// 7 character tool calls the LLM can make
// (update_soul / update_skill / update_memory /
// update_inventory / remove_inventory_item /
// set_currency / remove_currency).

func (t *Toolset) AppendSoul(characterDir, section, value string) (bool, error) {
	return t.Character.AppendSoul(characterDir, section, value)
}

func (t *Toolset) AppendSkill(characterDir, section, value string) (bool, error) {
	return t.Character.AppendSkill(characterDir, section, value)
}

func (t *Toolset) AppendMemorySection(characterDir, section, value string) (bool, error) {
	return t.Character.AppendMemorySection(characterDir, section, value)
}

func (t *Toolset) AppendInventoryItem(characterDir string, item charprofile.Item) (bool, error) {
	return t.Character.AppendInventoryItem(characterDir, item)
}

func (t *Toolset) RemoveInventoryItem(characterDir, name string) error {
	return t.Character.RemoveInventoryItem(characterDir, name)
}

func (t *Toolset) SetCurrency(characterDir, name string, count int) (bool, error) {
	return t.Character.SetCurrency(characterDir, name, count)
}

func (t *Toolset) RemoveCurrency(characterDir, name string) error {
	return t.Character.RemoveCurrency(characterDir, name)
}

// Reload flushes any in-memory caches. The file backend is
// stateless today (every read goes to disk) so the method
// is a no-op kept for interface compatibility — but having
// it explicit means the dispatcher can wire /reload today,
// and a future backend with an LRU cache can opt into a
// real invalidation without changing call sites.
func (t *Toolset) Reload() error {
	// No caches to invalidate. The method exists so the
	// toolset satisfies tools.Reloadable. Logging the
	// event so an operator who hits /reload sees something
	// in slowlog proving the path was taken.
	return nil
}

// Compile-time check: *Toolset must satisfy tools.Reloadable.
var _ tools.Reloadable = (*Toolset)(nil)

// (time is imported indirectly via time.Time fields on
// some signatures; reserved for future methods).
var _ time.Time
