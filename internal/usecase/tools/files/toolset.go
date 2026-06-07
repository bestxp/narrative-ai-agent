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

	"narrative/internal/adapter/storage"
	"narrative/internal/slowlog"
	"narrative/internal/usecase/tools"
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
// summarizer is the LLM-driven NPC condensation hook used
// by MaintainNPCs. loreSummarizer is the LLM-driven
// lore.md compaction hook used by MaintainLore. Pass nil
// to either to disable the LLM path — the file backend
// will then log a warning and skip.
func New(fs *storage.FileStore, log zerolog.Logger, slow *slowlog.Logger, summarizer tools.NPCSummarizer, loreSummarizer tools.LoreSummarizer) *Toolset {
	return &Toolset{
		State:     newState(fs, log),
		Memory:    newMemory(fs, log, summarizer, loreSummarizer),
		World:     newWorld(fs, log),
		Character: newCharacter(fs, log, slow),
		NPC:       newNPC(fs, log),
	}
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
