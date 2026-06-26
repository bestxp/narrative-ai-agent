package domain

import (
	"errors"
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// SystemState is the technical half of the bot's persistent
// memory. It is kept in system_state.md (sibling of state.md) and
// is meant for the operator, not the LLM: compaction history,
// session counters, last autosave hash. The bot appends to it
// on every compaction and on every successful autosave so the
// operator can grep the file and answer "what did the bot do
// between 12:30 and 14:00?" without trawling zerolog output.
//
// The struct is round-trip-safe: MarshalSystemState →
// WriteRawAtomic → ReadRaw → ParseSystemState yields a struct
// equal to the original. Field order is stable; new fields go
// at the end of each block.
type SystemState struct {
	// Session is bookkeeping about the current bot run. Bot
	// restart resets StartedAt; TurnCount is per-process.
	Session SessionState `yaml:"session"`
	// Compaction records every history-trim event. Append-only;
	// rotation happens at the writer (usecase/systemstate).
	Compaction CompactionLog `yaml:"compaction"`
	// Autosave tracks the most recent git commit/push and a
	// running total. Useful to confirm "did the bot actually
	// save what I think it saved".
	Autosave AutosaveState `yaml:"autosave"`
}

// SessionState is the running per-process counter block.
type SessionState struct {
	// StartedAt is the RFC3339 timestamp the bot process booted.
	StartedAt time.Time `yaml:"started_at"`
	// LastActive is the timestamp of the last IncomingMessage
	// the bot processed. Updated on every handleIncoming entry.
	LastActive time.Time `yaml:"last_active"`
	// TurnCount counts every IncomingMessage, including
	// commands. Distinct from FreeformTurnCount in the
	// auto-save trigger, which excludes /-prefixed messages.
	TurnCount int `yaml:"turn_count"`
	// FreeformTurnCount counts only messages with an empty
	// Command — i.e. the player's narrative turns.
	FreeformTurnCount int `yaml:"freeform_turn_count"`
	// ChatID is the active Telegram chat id (or any other
	// transport's primary chat). Useful for the operator to
	// confirm they are looking at the right conversation when
	// the file is shared between bots.
	ChatID string `yaml:"chat_id"`
	// Character / World mirror info.yaml for convenience —
	// having them in one place means the operator can answer
	// "who is the bot playing right now?" from a single file.
	Character string `yaml:"character"`
	World     string `yaml:"world"`
}

// CompactionLog is a capped append-only history of compactions.
// The writer keeps History at most MaxEntries long; older
// entries are evicted to keep the file small.
type CompactionLog struct {
	// TotalCompactions counts every compaction across the
	// lifetime of this file. Useful for "are we compacting
	// too often?" diagnostic.
	TotalCompactions int `yaml:"total_compactions"`
	// LastCompactionAt is the most recent compaction timestamp,
	// kept separately for cheap "did anything happen recently?"
	// checks.
	LastCompactionAt time.Time `yaml:"last_compaction_at"`
	// History is the ring of compaction events. Newer entries
	// go at the end; the writer trims from the front when the
	// slice exceeds MaxEntries.
	History []CompactionEvent `yaml:"history"`
}

// CompactionEvent is one row in the compaction log. Fields are
// the "before / after" snapshot so a later operator can see
// "the bot was about to hit 22k tok and decided to drop 23
// turns" without re-running the session.
type CompactionEvent struct {
	// At is the RFC3339 timestamp the compaction happened.
	At time.Time `yaml:"at"`
	// Trigger is a short label describing why the bot fired the
	// compaction. Today only "context_window*0.7" exists;
	// future heuristics (e.g. "npc_compaction_threshold")
	// will get their own labels.
	Trigger string `yaml:"trigger"`
	// Role is the LLM role that was over budget (e.g. "narrative").
	Role string `yaml:"role"`
	// BeforeTokens is the size of the conversations[]+system
	// prompt that triggered the compaction. Round number for
	// planning purposes.
	BeforeTokens int `yaml:"before_tokens"`
	// AfterTokens is the same measure after the drop. The
	// difference is the saving.
	AfterTokens int `yaml:"after_tokens"`
	// DroppedTurns is the count of (user/assistant/tool) turn
	// groups removed from conversations[].
	DroppedTurns int `yaml:"dropped_turns"`
	// KeptRecent is the keep_recent value the bot used. Lets
	// the operator see the threshold retroactively.
	KeptRecent int `yaml:"kept_recent"`
}

// AutosaveState records the most recent git commit/push plus a
// running counter. Updated by the auto-save path in main.go.
type AutosaveState struct {
	// LastHash is the short SHA of the most recent successful
	// commit. Empty when no autosave has happened yet.
	LastHash string `yaml:"last_hash"`
	// LastSaveAt is the timestamp of LastHash.
	LastSaveAt time.Time `yaml:"last_save_at"`
	// TotalSaves counts every successful autosave in this file's
	// history. Empty commits (no changes) are not counted.
	TotalSaves int `yaml:"total_saves"`
}

// SystemStateFile is the canonical on-disk name of the file
// that holds a SystemState. Sibling of info.yaml at the data
// root.
const SystemStateFile = "system_state.md"

// MarshalSystemState renders a SystemState as the YAML the
// system_state.md file is meant to hold. The renderer is small
// and stable: keys appear in the order they were declared in
// the struct tags.
func (s SystemState) MarshalSystemState() (string, error) {
	out, err := yaml.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("system_state: marshal: %w", err)
	}

	return string(out), nil
}

// ParseSystemState decodes a system_state.md body. An empty
// body or a structurally invalid document are both errors: a
// blank state file usually means the operator deleted it by
// mistake, and silently re-initialising would lose the audit
// trail.
func ParseSystemState(content string) (SystemState, error) {
	if content == "" {
		return SystemState{}, errors.New("system_state.md: empty document")
	}

	var s SystemState
	if err := yaml.Unmarshal([]byte(content), &s); err != nil {
		return SystemState{}, fmt.Errorf("system_state.md: parse: %w", err)
	}

	return s, nil
}

// DefaultSystemState returns the zero state. The operator can
// hand-edit system_state.md down to this shape; the bot will
// pick it up on the next read.
func DefaultSystemState() SystemState {
	return SystemState{}
}

// AppendCompactionEvent drops the oldest entries so History
// stays at most maxEntries long, then appends ev. The struct is
// modified in place; the returned value is the same pointer
// for convenience.
func (c *CompactionLog) AppendCompactionEvent(ev CompactionEvent, maxEntries int) {
	if maxEntries < 1 {
		maxEntries = 1
	}

	c.History = append(c.History, ev)
	c.TotalCompactions++

	c.LastCompactionAt = ev.At
	if len(c.History) > maxEntries {
		// Drop from the front. Slice reallocation is cheap at
		// these sizes (single digits).
		c.History = c.History[len(c.History)-maxEntries:]
	}
}
