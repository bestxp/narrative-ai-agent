// Package limits is the single source of truth for the
// project's tunable thresholds. Three packages
// (prompts, npcprofile, staging) used to each carry
// their own local copy of the same constants; this
// package centralises them so a single edit propagates
// to all consumers — the LLM-facing prompt templates,
// the auto-deflate logic, and the staging renderer all
// see the same value.
//
// Why a separate package:
//
//   - prompts.Render is called by npcprofile's
//     BuildMarkdown path, so prompts and npcprofile
//     cannot import each other transitively. Without
//     a third package, any shared constant had to be
//     duplicated (with a unit test pinning the two
//     copies together).
//   - staging.MaxStageRenderBytes has the same role
//     (a render cap) but lives in a different
//     package that has no business depending on
//     npcprofile or prompts.
//
// The package is intentionally tiny — only the
// constants that are shared across packages belong
// here. Per-package, per-domain limits (e.g. a
// character-memory target in chars/profile) stay in
// their owning package; this is the cross-cutting
// set only.
package limits

// NPCPersonalMemoryLimit is the soft cap on the
// number of personal-memory facts an NPC profile
// may carry before the auto-deflate hook (called
// from end_day) trims it down. Used by:
//
//   - prompts.DefaultNPCPersonalMemoryTarget (the
//     LLM sees this in npc_summary.md.tmpl).
//   - npcprofile.NPCPersonalMemoryLimit (the
//     dispatcher-side threshold).
const NPCPersonalMemoryLimit = 25

// NPCPersonalMemoryTarget is the upper bound the
// summarizer aims for when defragmenting an over-
// grown NPC profile. The path
// "personal_memory: 25 → ≤20" is encoded directly
// in the template as {{ .Compaction.NPCPersonalMemoryLimit }} → ≤{{ .Compaction.NPCPersonalMemoryTarget }}.
const NPCPersonalMemoryTarget = 20

// MemoryTargetBytes is the soft cap on
// characters/<dir>/memory.yaml — the active
// character's cross-multiverse memory. The end-of-day
// defrag (summarizer) re-files facts into the 4
// canonical buckets when the file exceeds this size.
// 4 KB = 4096 bytes; the threshold is in bytes, not
// runes, because file size is what hits the disk.
const MemoryTargetBytes = 4096

// LoreLineLimit is the soft cap on worlds/<w>/lore.md
// line count. When the file exceeds this, maintain_lore
// (or the end-of-day hook) compresses the older
// entries.
const LoreLineLimit = 500

// LoreTargetLines is the line-count target the
// lore-summarizer aims for when defragmenting lore.md.
const LoreTargetLines = 250

// MemoriseWindowDays is the size of the rolling
// window the memorise-summarizer compresses. The hook
// fires when a day closes a 30-day boundary (or any
// wider multiple — 60, 90, ...).
const MemoriseWindowDays = 30

// MemoriseSentencesPer30Days is the prose-density
// target the memorise-summarizer aims for. A 30-day
// window should compress into roughly this many
// sentences (a wider window scales linearly).
const MemoriseSentencesPer30Days = 10

// StageRenderMaxBytes is the render cap for the
// active stage of a world's staged story graph.
// The renderer in staging.Render trims the rendered
// output to this size; excess is replaced with a
// truncation marker so the LLM gets a deterministic
// cap regardless of how verbose the operator
// authored a stage description.
const StageRenderMaxBytes = 2000

// ProtocolWindowDays is the maximum number of
// day-entries the end-of-day protocol section in
// state.md may keep before the oldest is evicted to
// memorise.md.
const ProtocolWindowDays = 2

// ProtocolMaxChars is the size cap (in characters,
// not bytes) of the entire "## Протокол прошедших
// дней" section. The window-enforcement pass evicts
// the oldest day if either the day count OR this char
// count grows past the cap.
const ProtocolMaxChars = 5000

// InPlaceSummaryWordsMin/Max is the word-count range
// the in-place compaction prompt tells the LLM to aim
// for when compressing the current day's dialog into
// "## Хроника текущего дня". The range is soft; the
// model is told "150-300 слов" verbatim via
// {{ .Compaction.InPlaceSummaryWordsMin }}-{{ .Compaction.InPlaceSummaryWordsMax }}.
const (
	InPlaceSummaryWordsMin = 150
	InPlaceSummaryWordsMax = 300
)

// EndOfDaySummaryWordsMin/Max is the word-count range
// the end-of-day protocol prompt tells the LLM to aim
// for when compressing a closed day into
// "## Протокол прошедших дней". Referenced via
// {{ .Compaction.EndOfDaySummaryWordsMin }}-{{ .Compaction.EndOfDaySummaryWordsMax }}.
const (
	EndOfDaySummaryWordsMin = 200
	EndOfDaySummaryWordsMax = 400
)

// OldTurnsSummaryTokensMin/Max is the token-count
// range the old-turns compaction prompt tells the LLM
// to aim for when compressing dropped conversation
// turns into a fact log appended to state.md. Referenced
// via {{ .Compaction.OldTurnsSummaryTokensMin }}-{{ .Compaction.OldTurnsSummaryTokensMax }}.
const (
	OldTurnsSummaryTokensMin = 200
	OldTurnsSummaryTokensMax = 400
)

// LoreTargetLinesMin/Max is the line-count range the
// lore-summary prompt tells the LLM to compress lore.md
// down to. The soft target is LoreTargetLines (250); the
// range is the acceptable band around it. Referenced
// via {{ .Compaction.LoreTargetLinesMin }}-{{ .Compaction.LoreTargetLinesMax }}.
const (
	LoreTargetLinesMin = 200
	LoreTargetLinesMax = 300
)

// LoreSectionTargetMin/Max is the "примерно N-M секций"
// range the lore-summary prompt mentions alongside the
// line-count target. Referenced via
// {{ .Compaction.LoreSectionTargetMin }}-{{ .Compaction.LoreSectionTargetMax }}.
const (
	LoreSectionTargetMin = 30
	LoreSectionTargetMax = 50
)

// MemoriseSentenceMin/Max is the acceptable band around
// MemoriseSentencesPer30Days (the soft target per 30-day
// window). The memorise-summary prompt tells the LLM
// that fewer than Min is "too thin" and more than Max is
// "you didn't compress". Referenced via
// {{ .Compaction.MemoriseSentenceMin }} / {{ .Compaction.MemoriseSentenceMax }}.
const (
	MemoriseSentenceMin = 6
	MemoriseSentenceMax = 15
)

// NarrativeWordLimitFloor is the floor the narrative
// prompt mentions as the lower bound of acceptable
// narration length ("80-{{ .Narrative.WordLimit }} слов").
// The ceiling is the operator-configured WordLimit; the
// floor is a fixed hint that "under this is too short".
const NarrativeWordLimitFloor = 80
