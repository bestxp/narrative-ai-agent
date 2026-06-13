// Package tools is the single interface every usecase-level
// caller (the GM, the dispatcher, future transports) depends
// on. The Tool interface bundles the file-backed domain
// operations the bot performs in response to model tool calls
// or to player /commands:
//
//   - state.md / plan.md / memorise.md lifecycle (UpdateState,
//     AppendEvent, AppendHistoryToState, ArchiveDay, RotatePlan)
//   - NPC creation and profile reads (Create, Load)
//   - world transitions (/leave, /return)
//   - character files (Append, Read) for SOUL.md / SKILL.md /
//     memory.md
//   - memory + lore + NPC condensation (AppendMemory, AppendLore,
//     CompactNPCs)
//
// The interface lives in this package; the file-backed
// implementation is in internal/usecase/tools/files. A future
// backend (s3, git, in-memory) just implements the same Tool
// interface — no need to satisfy five narrow interfaces in
// lockstep.
//
// Per-concern split is preserved at the *implementation* level
// (files.State, files.Memory, files.World, files.Character,
// files.NPC) so each concern is a self-contained file with
// its own zerolog component. The Tool struct in
// internal/usecase/tools/files glues them together; callers
// that want one concern can hold a narrower field reference
// (files.Toolset exposes each as a separate field).
package tools

import (
	"context"
	"time"

	"github.com/bestxp/narrative-ai-agent/internal/charprofile"
)

// StateSnapshot is the trimmed "here and now" written to
// state.md. AppendEvents is the day's log: each entry is
// appended on every UpdateState call, and the section is
// cleared on ArchiveDay.
type StateSnapshot struct {
	World        string
	Day          int
	InFlight     bool
	Location     string
	Moment       string
	NPCs         []string
	AppendEvents []string
}

// NPCSearchResult is the compact view returned by
// SearchNPC: a display_name + slug + a 1-2 sentence
// temperament + current_status. The Tool interface
// keeps the type in the tools package (rather than
// aliasing files.SearchResult) so the interface stays
// decoupled from the filesystem backend; the file
// implementation converts at the boundary.
type NPCSearchResult struct {
	DisplayName   string `json:"display_name"`
	Slug          string `json:"slug"`
	Temperament   string `json:"temperament,omitempty"`
	CurrentStatus string `json:"current_status,omitempty"`
	Source        string `json:"source"`
}

// EndSceneResult mirrors files.EndSceneResult in the
// tools package. Kept as a separate type so the
// interface stays decoupled from the filesystem
// backend (same rationale as NPCSearchResult above).
type EndSceneResult struct {
	KeptNPCs      []string
	PrunedNPCsLen int
}

// NPCLOD is the level-of-detail knob for LoadLOD. The
// numeric values are stable: callers and tests may
// compare them directly. The values are the same as
// the canonical "small / medium / large" tiers in
// the prompt-cache budget (loadActiveNPCs applies the
// mapping in gm.go — this enum is the wire surface
// only).
type NPCLOD int

const (
	// LODFull is the markdown render with every section
	// (BuildMarkdown). Default for the model's main
	// interlocutor + a small cast of sidekicks.
	LODFull NPCLOD = iota
	// LODCompact drops the big arrays (abilities,
	// personal_memory, critical_knowledge) and keeps
	// temperament + relations + current_status. Used
	// for side characters in mid-size casts.
	LODCompact
	// LODOneLine is a single line per NPC: name +
	// 1-sentence temperament + 1-sentence status. Used
	// for background characters in 10+ NPC scenes.
	LODOneLine
)

// LeaveResult is the outcome of a Leave call. FromWorld /
// FromDay are the world we left, NewWorldInit is true when the
// destination had to be created from scratch.
type LeaveResult struct {
	FromWorld    string
	FromDay      int
	NewWorld     string
	NewWorldInit bool
}

// NPCProfile is the per-NPC fact bundle the GM hands to the
// create tool. DisplayName is the localised label, File is
// the latin slug (post-SanitizeName). Nicknames are optional.
// NPCProfile is the bundle the create_npc tool accepts on
// first appearance. Each field maps 1:1 to a section in
// worlds/<w>/characters/<slug>.md; the renderer in BuildNPCMarkdown
// emits them in a fixed order. The fields are intentionally
// verbose: the model knows a lot about the NPC at the moment
// of first appearance (the player just met them, the
// narrative described them in detail) and a one-line
// "Хокаге-старый, добрый" profile is not enough to drive
// scenes later. The more structure we get on day one, the
// more the GM can lean on this profile when the player
// returns to the same NPC twenty sessions from now.
type NPCProfile struct {
	DisplayName       string
	File              string
	Nicknames         []string
	Temperament       string
	Relations         string   // free text, multi-line — "## Отношения с ГГ\n..."
	Abilities         []string // flat list — "- ability", "- ability"
	PersonalMemory    string   // free text — "## Личная память/факты\n..."
	CurrentStatus     string   // free text — "## Текущий статус\n..."
	CriticalKnowledge string   // free text — "## Критические знания\n..."
	// LastUpdate is a free-form tag (the model usually writes
	// "День N — короткое событие"). The renderer pins it
	// to the bottom of the profile as "## Последнее
	// обновление".
	LastUpdate string
}

// CharacterSnapshot is the read-only bundle /me
// renders. It is pre-formatted for plain-text
// output; markdown sections pass through unchanged.
//
// The h5 refactor added Inventory — the per-character
// inventory.yaml is now part of the snapshot, so
// the operator can see the GG's wallet and pockets
// in a single /me call.
type CharacterSnapshot struct {
	Character string
	World     string
	SOUL      string
	SKILL     string
	Memory    string
	Inventory string
	State     string
	Day       int
}

// Tool is the single interface the GM and dispatcher depend
// on. Every method is one tool the model can call, one
// /command the player can run, or one internal hook the
// summariser / state machine needs.
//
// Naming convention: methods that match an LLM tool use the
// tool's canonical name in PascalCase (UpdateState, ArchiveDay,
// Create, Load, Leave, ReturnWorld, Append, Read, CompactNPCs).
// Methods that are pure plumbing (AppendEvent,
// AppendHistoryToState) keep the same PascalCase convention
// but have no LLM tool counterpart.
type Tool interface {
	// --- state.md / plan.md / memorise.md ---
	UpdateState(snap StateSnapshot) error
	AppendEvent(text string) error
	AppendHistoryToState(world, summary string, at time.Time) error
	// ArchiveDay appends a new day entry to memorise.md and
	// triggers automatic 30-day window compression when the
	// recorded day closes a window (day % 30 == 0, or any
	// wider timeskip). The context carries the request
	// deadline — the compression step is an LLM call that
	// may take a few seconds, so the per-turn deadline
	// applies. The summarizer is wired in cmd/bot/main.go
	// (see tools.MemoriseSummarizer); nil summarizers are
	// tolerated and the call is logged + skipped.
	ArchiveDay(ctx context.Context, world string, day int, summary string) error
	// EndScene closes the current scene without closing
	// the day. It prunes the active roster to the
	// permanent_party subset (nil = no prune) so the
	// next turn rebuilds the scene around a smaller
	// cast. The GM resets the per-chat conversation
	// history and invalidates the world snapshot after
	// the call returns; the file backend only touches
	// the roster.
	EndScene(world string, permanentParty []string) (*EndSceneResult, error)
	RotatePlan(world string, events []string) error

	// --- memory.md / lore.md / NPC condensation ---
	AppendMemory(character, line string) error
	AppendLore(world, header, bullet string) error
	// MaintainNPCs walks the active world's NPC files
	// and asks the LLM-driven summarizer to compact
	// any profile whose personal_memory has grown past
	// the threshold (see npcprofile.NPCPersonalMemoryLimit).
	// Returns the list of NPC display names that were
	// actually rewritten. The implementation in
	// files.Memory takes a Summarizer dependency (see
	// NPCSummarizer interface) which is wired at
	// construction time in cmd/bot/main.go.
	MaintainNPCs(world string) ([]string, error)
	// MaintainLore compacts the active world's lore.md
	// when it grows past tools.LoreMaintainThreshold
	// (500 lines by default). Returns true when the
	// file was rewritten. canon.md is NEVER touched —
	// it is operator-owned, the bot only reads it.
	// (Lore is the only world-scoped file the bot
	// writes via summarizer; canon stays external.)
	// The context carries the request deadline (the
	// GM is in a per-turn deadline, /maintenance is
	// operator-triggered with a longer one).
	MaintainLore(ctx context.Context, world string) (bool, error)
	// MaintainCharacterMemory defragments the active
	// character's memory.yaml when it grows past
	// tools.CharacterMemoryMaintainBytes (4KB by
	// default). Called from the end-of-day pass
	// AFTER MaintainNPCs — a long-running campaign
	// does not bloat the prompt with the same fact
	// rewritten 30 times. The summarizer refiles
	// legacy free-form sections (## Действия дня 1,
	// ## Видения Кагуи, etc.) into the 4 canonical
	// buckets ("Яркие моменты", "Факты о мире",
	// "Обещания и цели", "Важные люди"). Returns
	// true when the file was rewritten. The context
	// carries the per-turn deadline (the call is a
	// single LLM round-trip).
	MaintainCharacterMemory(ctx context.Context, world, character string) (bool, error)

	// --- world transitions ---
	Leave(fromWorld, toWorld, skipNote, character string) (*LeaveResult, error)
	ReturnWorld(world, days string) (string, error)

	// --- character files (h5 refactor) ---
	//
	// The legacy single Append(file=...) dispatcher
	// was replaced with one method per file kind.
	// The split removes the stringy `file` argument
	// (one of SOUL / SKILL / memory) that was easy
	// to typo, and adds explicit inventory ops so
	// the model does not have to encode REPLACE /
	// delete semantics in a "magic" append payload.
	AppendSoul(characterDir, section, value string) (bool, error)
	AppendSkill(characterDir, section, value string) (bool, error)
	// AppendMemorySection is the per-NPC-name collision
	// avoidance: the *Memory concern (memorise.md
	// inside worlds) already has its own AppendMemory
	// method on a different type. They are reachable
	// from different call sites — a `*Toolset` in
	// production code wires the *Character one into
	// the per-character file path and the *Memory one
	// into the world-state path.
	AppendMemorySection(characterDir, section, value string) (bool, error)
	// AppendInventoryItem is REPLACE-on-name. Same
	// name = overwrite description/equip/special.
	AppendInventoryItem(characterDir string, item charprofile.Item) (bool, error)
	RemoveInventoryItem(characterDir, name string) error
	SetCurrency(characterDir, name string, count int) (bool, error)
	RemoveCurrency(characterDir, name string) error
	// Read returns the snapshot of the current
	// character (the four YAML files + state.md).
	Read(activeChar, activeWorld string) (*CharacterSnapshot, error)

	// --- NPC profiles ---
	Create(world string, p NPCProfile) error
	// Load returns the full markdown render of an NPC
	// profile. For multi-NPC scenes where the cache
	// budget is tight, prefer LoadLOD with
	// LODCompact / LODOneLine so the world block stays
	// under the prompt-cache threshold.
	Load(world, npc string) (string, error)
	// LoadLOD is the same as Load but with an explicit
	// level-of-detail knob. The file backend reads the
	// profile once and renders it at the requested
	// detail; future loads of the same NPC at the same
	// LOD still pay the read cost (the LOD layer is in
	// the caller, not the backend — see the
	// loadActiveNPCs comment for the rationale).
	LoadLOD(world, npc string, lod NPCLOD) (string, error)
	// SearchNPC resolves a free-form query against the
	// world's NPC registry and returns a compact
	// description (display_name + temperament +
	// current_status). Used by the search_npc tool when
	// the model needs an NPC that is not already in the
	// active roster. Implementations may rate-limit or
	// re-use a registry cache; the dispatcher still
	// applies its own in-memory dedupe on top.
	SearchNPC(world, query string) (*NPCSearchResult, error)
	// UpdateNPC appends fresh facts to an existing NPC
	// profile. The section is one of the canonical NPC
	// section names (case-insensitive match); the
	// "section" argument carries the new lines. Use this
	// tool whenever the model observes something new
	// about an NPC: a new ability, a status change, a
	// new relationship, a new fact, a new critical
	// knowledge. The "last_update" line is refreshed
	// automatically from "now". Returns ErrNPCNotFound
	// when the NPC has no profile yet — the model must
	// call Create first.
	UpdateNPC(world, npc, section, appendText string) error
}

// Reloadable is an optional capability a backend can
// implement. /reload type-asserts the wired Tool to this
// interface and calls Reload() to force the next turn to
// pick up data from the canonical source (e.g. re-read
// from disk after an operator edited a file by hand, or
// pull fresh lore from a remote in some future deployment).
//
// The file backend implements it as a no-op today — every
// read is live. A future cached backend would invalidate
// its LRU here. The capability is opt-in so test doubles
// and pure in-memory mocks can skip the method.
type Reloadable interface {
	Reload() error
}
