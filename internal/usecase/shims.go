// Package usecase is the application-orchestration layer.
// File-backed domain operations live in
// internal/usecase/tools/files behind the single
// tools.Tool interface. This file re-exports the public
// types so callers (cmd/bot/main.go, dispatcher, gm) can
// keep using the usecase.* namespace.
package usecase

import (
	"time"

	"github.com/rs/zerolog"

	"github.com/bestxp/narrative-ai-agent/internal/adapter/storage"
	"github.com/bestxp/narrative-ai-agent/internal/slowlog"
	"github.com/bestxp/narrative-ai-agent/internal/usecase/tools"
	"github.com/bestxp/narrative-ai-agent/internal/usecase/tools/files"
)

// Tool re-exports tools.Tool — the single interface the
// GM and dispatcher depend on. A file-backed implementation
// is constructed by NewFileToolset; tests can pass a mock.
type Tool = tools.Tool

// Reloadable re-exports tools.Reloadable. /reload type-asserts
// the wired Tool to this interface before calling Reload.
type Reloadable = tools.Reloadable

// StateSnapshot re-exports tools.StateSnapshot.
type StateSnapshot = tools.StateSnapshot

// LeaveResult re-exports tools.LeaveResult.
type LeaveResult = tools.LeaveResult

// NPCProfile re-exports tools.NPCProfile.
type NPCProfile = tools.NPCProfile

// CharacterSnapshot re-exports tools.CharacterSnapshot.
type CharacterSnapshot = tools.CharacterSnapshot

// FileToolset re-exports files.Toolset so callers can refer
// to the concrete file-backed implementation without
// importing the files package directly.
type FileToolset = files.Toolset

// FileBackend identifies the file-backed implementation.
const FileBackend = "files"

// NewFileToolset is the canonical entry point for the
// file-backed toolset. main.go calls this once and hands
// the result to gm and dispatcher. The returned *files.Toolset
// satisfies tools.Tool and tools.Reloadable.
//
// summarizer is the LLM-driven NPC condensation hook used
// by MaintainNPCs. loreSummarizer is the LLM-driven
// lore.md compaction hook used by MaintainLore.
// chronicleSummarizer is the LLM-driven 30-day window
// compression hook used by ArchiveChronicleDay (automatic on
// day%30==0 and on timeskips). characterMemorySummarizer
// is the LLM-driven memory.yaml defragmentation hook
// used by MaintainCharacterMemory (end-of-day pass).
// Pass nil to any of them to disable the LLM path — the
// file backend will then log a warning and skip. Tests
// typically pass nil for all four (or stubs).
func NewFileToolset(fs *storage.FileStore, log zerolog.Logger, slow *slowlog.Logger, summarizer tools.NPCSummarizer, loreSummarizer tools.LoreSummarizer, chronicleSummarizer tools.ChronicleSummarizer, characterMemorySummarizer tools.CharacterMemorySummarizer) *files.Toolset {
	return files.New(fs, log, slow, summarizer, loreSummarizer, chronicleSummarizer, characterMemorySummarizer)
}

// --- format / threshold / header helpers ---------------------------------

// FormatCharacterSnapshot is the dispatcher-friendly wrapper
// around the file backend's snapshot formatter.
func FormatCharacterSnapshot(s *CharacterSnapshot, maxPerSection int) string {
	return files.FormatSnapshot(s, maxPerSection)
}

// StateHeader returns the canonical "День N (в процессе|завершён)"
// line.
func StateHeader(day int, inFlight bool) string {
	return files.StateHeader(day, inFlight)
}

// NPCCompactLineThreshold is the line count at which
// /maintenance triggers a per-NPC compact.
const NPCCompactLineThreshold = files.NPCCompactLineThreshold

// ExtractDayNumber parses "День N" out of a state.md body.
func ExtractDayNumber(body string) (int, bool) {
	return files.ExtractDayNumber(body)
}

// AppendHistoryFunc is the function type summarizer.go and
// dispatcher.go use to record a compaction summary into
// state.md. The actual implementation lives on
// tools.Tool; this alias keeps the field names short.
type AppendHistoryFunc func(world, summary string, at time.Time) error
