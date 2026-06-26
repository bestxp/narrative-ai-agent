// Package usecase is the application-orchestration layer.
// File-backed domain operations live in
// internal/usecase/tools/files behind the single
// tools.Tool interface. This file re-exports the public
// types so callers (cmd/bot/main.go, dispatcher, gm) can
// keep using the usecase.* namespace.
package usecase

import (
	"time"

	"github.com/bestxp/narrative-ai-agent/internal/adapter/storage"
	"github.com/bestxp/narrative-ai-agent/internal/repository/api"
	"github.com/bestxp/narrative-ai-agent/internal/slowlog"
	"github.com/bestxp/narrative-ai-agent/internal/usecase/tools"
	"github.com/bestxp/narrative-ai-agent/internal/usecase/tools/files"
	"github.com/rs/zerolog"
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

// NewFileToolset is the canonical entry point for the
// file-backed toolset. main.go calls this once and hands
// the result to gm and dispatcher.
//
// fs is the legacy file-storage backend used by the
// raw-file paths (legacy migration, /me snapshot reads).
// repos is the canonical write path — every persistent
// write goes through a repository, not through fs.
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
func NewFileToolset(fs *storage.FileStore, repos *api.Repositories, log zerolog.Logger, slow *slowlog.Logger, summarizer tools.NPCSummarizer, loreSummarizer tools.LoreSummarizer, chronicleSummarizer tools.ChronicleSummarizer, characterMemorySummarizer tools.CharacterMemorySummarizer) *files.Toolset {
	return files.New(fs, repos, log, slow, summarizer, loreSummarizer, chronicleSummarizer, characterMemorySummarizer)
}

// --- format / threshold / header helpers ---------------------------------

// FormatCharacterSnapshot is the dispatcher-friendly wrapper
// around the file backend's snapshot formatter.
func FormatCharacterSnapshot(s *CharacterSnapshot, maxPerSection int) string {
	return files.FormatSnapshot(s, maxPerSection)
}

// AppendHistoryFunc is the function type summarizer.go and
// dispatcher.go use to record a compaction summary into
// state.md. The actual implementation lives on
// tools.Tool; this alias keeps the field names short.
type AppendHistoryFunc func(world, summary string, at time.Time) error
