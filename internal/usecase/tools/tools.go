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

import "time"

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
	DisplayName string
	File        string
	Nicknames   []string
	Temperament string
	Relations   string   // free text, multi-line — "## Отношения с ГГ\n..."
	Abilities   string   // free text, multi-line — "## Способности\n..."
	PersonalMemory string // free text — "## Личная память/факты\n..."
	CurrentStatus  string // free text — "## Текущий статус\n..."
	CriticalKnowledge string // free text — "## Критические знания\n..."
	// LastUpdate is a free-form tag (the model usually writes
	// "День N — короткое событие"). The renderer pins it
	// to the bottom of the profile as "## Последнее
	// обновление".
	LastUpdate string
}

// CharacterSnapshot is the read-only bundle /me renders. It
// is pre-formatted for plain-text output; markdown sections
// pass through unchanged.
type CharacterSnapshot struct {
	Character string
	World     string
	SOUL      string
	SKILL     string
	Memory    string
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
	ArchiveDay(world string, day int, summary string) error
	RotatePlan(world string, events []string) error

	// --- memory.md / lore.md / NPC condensation ---
	AppendMemory(character, line string) error
	AppendLore(world, header, bullet string) error
	CompactNPCs(world string) ([]string, error)

	// --- world transitions ---
	Leave(fromWorld, toWorld, skipNote, character string) (*LeaveResult, error)
	ReturnWorld(world, days string) (string, error)

	// --- character files ---
	Append(characterDir, file, section, appendText string) error
	Read(activeChar, activeWorld string) (*CharacterSnapshot, error)

	// --- NPC profiles ---
	Create(world string, p NPCProfile) error
	Load(world, npc string) (string, error)
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
