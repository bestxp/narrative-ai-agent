// Package files is the filesystem-backed implementation of
// the tools.Tool interface. It is the only "real" backend the
// production binary uses today; the package boundary keeps
// the adapter isolated so a future s3 / git / in-memory
// backend can drop in without touching gm.go or dispatcher.go.
package files

import (
	"context"
	"time"

	"github.com/rs/zerolog"

	"github.com/bestxp/narrative-ai-agent/internal/adapter/storage"
	"github.com/bestxp/narrative-ai-agent/internal/charprofile"
	"github.com/bestxp/narrative-ai-agent/internal/slowlog"
	"github.com/bestxp/narrative-ai-agent/internal/usecase/tools"
)

// Toolset is the file-backed Tool implementation. It
// embeds five small structs — one per concern — each holding
// its own *storage.FileStore reference. Splitting the impl
// across five files keeps the cognitive load down: state.go
// is the state's contract, npc.go the NPC's, etc.
//
// Toolset itself satisfies tools.Tool: every interface method
// is implemented as a thin forwarder to the appropriate
// embedded concern. Tests that want to mock a single concern
// (e.g. just the NPC tool) hold a reference to the
// corresponding field.
type Toolset struct {
	*State
	*Memory
	*World
	*Character
	*NPC
}

// New constructs the file-backed toolset. fs is shared
// across all five concerns; log is component-tagged per
// concern when needed (so a maintenance event and an NPC
// event get different "component" fields in zerolog). slow
// is the optional audit log; pass slowlog.Discard() in tests.
//
// summarizer is the LLM-driven NPC condensation hook used
// by MaintainNPCs. loreSummarizer is the LLM-driven lore.md
// compaction hook used by MaintainLore. memoriseSummarizer
// is the LLM-driven 30-day window compression hook used by
// ArchiveDay. characterMemorySummarizer is the LLM-driven
// memory.yaml defragmentation hook used by
// MaintainCharacterMemory. Pass nil to any of them to
// disable the LLM path — the file backend will then log a
// warning and skip.
func New(fs *storage.FileStore, log zerolog.Logger, slow *slowlog.Logger, summarizer tools.NPCSummarizer, loreSummarizer tools.LoreSummarizer, memoriseSummarizer tools.MemoriseSummarizer, characterMemorySummarizer tools.CharacterMemorySummarizer) *Toolset {
	mem := newMemory(fs, log, summarizer, loreSummarizer, memoriseSummarizer, characterMemorySummarizer)
	st := newState(fs, log)
	// Wire the post-ArchiveDay hook so the state writer
	// does not need a direct reference to the memory
	// struct. The hook is nil-safe — it logs and skips
	// when no summarizer is wired.
	st.SetMemoriseCompress(mem.memoriseCompressAfterArchive)
	return &Toolset{
		State:     st,
		Memory:    mem,
		World:     newWorld(fs, log),
		Character: newCharacter(fs, log, slow),
		NPC:       newNPC(fs, log),
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
	res, err := t.NPC.Search(world, query)
	if err != nil {
		return nil, err
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

// unused import guard: the time package is referenced by
// method signatures in state.go (AppendHistoryToState
// takes a time.Time), but only indirectly through this
// file. Pulling the import here keeps the linter happy if
// state.go ever drops the import.
var _ = time.Time{}
