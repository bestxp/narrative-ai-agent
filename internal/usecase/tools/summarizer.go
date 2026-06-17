package tools

import "context"

// NPCSummarizer is the LLM-driven compaction hook the
// file-backed NPC maintainer calls when a profile's
// personal_memory list has grown past the threshold
// (npcprofile.NPCPersonalMemoryLimit). It lives in
// the tools package as a small, focused interface so
// the file implementation (files.Memory) can be wired
// against any summarizer — the production usecase-based
// one in usecase.Summarizer, or a stub in tests.
//
// The summarizer is given a display name (so the LLM
// can address the NPC in its system prompt), the
// current profile body in YAML form, and the world's
// chronicle tail for context. It returns a NEW YAML
// body (or the original, if it could not compress
// further). The contract is:
//
//  1. The returned body MUST be parseable as a
//     npcprofile.Profile (YAML). If the LLM emits
//     invalid YAML the caller logs a warning and
//     leaves the original file untouched.
//  2. The summarizer MUST NOT call back into the
//     tools layer (no AppendLore, no UpdateNPC). It
//     receives the world name + chronicle tail
//     purely as read context, not as a write handle.
//  3. The summarizer is best-effort. It may decide
//     the profile is already tight (returns the
//     input unchanged) — callers must not assume
//     every call produces a smaller body.
//  4. The summarizer is invoked synchronously by the
//     maintenance tool. The implementation may
//     respect ctx cancellation.
type NPCSummarizer interface {
	SummarizeNPC(ctx context.Context, displayName, world string, yamlBody, chronicleTail []byte) ([]byte, error)
}

// LoreSummarizer is the LLM-driven compaction hook
// for the file-backed lore maintainer. It is invoked
// when a world's lore.md grows past LoreMaintainThreshold
// (500 lines by default). The contract mirrors
// NPCSummarizer:
//
//  1. The returned body MUST be the same markdown
//     format the dispatcher expects ("## header"
//     sections + "- bullet" lines). If the LLM emits
//     something else the caller logs a warning and
//     leaves the file alone.
//  2. The summarizer MUST NOT call back into the
//     tools layer. It receives chronicle tail and
//     world state for read-only context, no write handle.
//  3. Best-effort: returns the input unchanged when
//     the summarizer could not compress.
//  4. Respects ctx cancellation.
type LoreSummarizer interface {
	SummarizeLore(ctx context.Context, world string, loreBody, chronicleTail, stateMD []byte) ([]byte, error)
}

// ChronicleSummarizer is the LLM-driven compaction hook
// for the file-backed chronicle maintainer. It is
// invoked automatically after ArchiveChronicleDay
// records a day that closes a window, and on any larger
// timeskip (the caller computes the actual day range
// to compact, not a fixed 30).
//
// The summarizer receives the world name, the day
// range being collapsed (start, end, both inclusive),
// and the WHOLE current chronicle file as read
// context. The whole-file context is important: the
// model needs the earlier, already-compressed
// summaries to keep its prose consistent (it must not
// invent facts that contradict an earlier window, and
// it should dedupe any cross-window repetitions such
// as a 15-day training arc that spans the boundary).
//
// The contract mirrors NPCSummarizer / LoreSummarizer:
//
//  1. The returned body MUST be the distilled memory
//     text for the [start..end] window. The caller
//     appends it as a new Period entry; the raw day
//     entries for that range are dropped. Empty output
//     means "no compression happened" and the caller
//     skips.
//  2. The summarizer MUST NOT call back into the tools
//     layer. It is read-only over the supplied context.
//  3. Best-effort: the model may decide the window is
//     already too thin to compress (e.g. only 5 days of
//     real activity in a 30-day window) and return an
//     empty body — the caller treats that as "no
//     compression happened" and skips.
//  4. The summarizer SHOULD aim for ~10 sentences for
//     a 30-day window, scaling roughly linearly for
//     wider windows (60 days → ~20 sentences,
//     90 days → ~30). The exact target is in the
//     system prompt.
//  5. Respects ctx cancellation.
type ChronicleSummarizer interface {
	SummarizeChronicle(ctx context.Context, world string, startDay, endDay int, fullChronicle string) ([]byte, error)
}

// CharacterMemorySummarizer is the LLM-driven
// compaction hook for the active character's
// memory.yaml. Invoked from GM.EndOfDay AFTER the
// NPC maintenance pass — the character memory is
// de-fragmented, deduped, and re-filed into the
// canonical 4-section enum ("Яркие моменты",
// "Факты о мире", "Обещания и цели", "Важные
// люди") so a long-running campaign does not
// bloat the prompt.
//
// The summarizer receives the world name (for
// context — what canon is in play), the character
// display name, the current memory.yaml body in
// YAML form, and the world's chronicle tail
// (for "which days are key" context). The returned
// body is a NEW memory.yaml in the canonical
// `data: [{section, values}]` shape.
//
// The contract mirrors NPCSummarizer /
// LoreSummarizer:
//
//  1. The returned body MUST be parseable as a
//     charprofile.Memory. If the LLM emits invalid
//     YAML the caller logs a warning and leaves the
//     file untouched (a fresh .bak covers the
//     pre-rewrite body as the operator recovery
//     path).
//  2. The summarizer is READ-ONLY over the
//     supplied context (it has no write handle, by
//     interface). It MUST NOT add or invent facts
//     that are not in the input body or chronicle
//     tail.
//  3. The summarizer is best-effort. It may decide
//     the file is already tight and return the
//     input unchanged — callers must not assume a
//     shrink happens every call.
//  4. The summarizer SHOULD refile legacy free-form
//     sections ("## Действия дня 1", "## Видения
//     Кагуи", etc.) into one of the 4 canonical
//     buckets. Anything that does not fit ("Эмоции",
//     "Эволюция") is folded into "Яркие моменты"
//     where appropriate or dropped as transient
//     mood.
//  5. The summarizer SHOULD drop redundant day-of
//     references (the player's chronology is in the
//     chronicle — duplicating "День 5" inside a
//     memory fact is noise).
//  6. Respects ctx cancellation.
type CharacterMemorySummarizer interface {
	SummarizeCharacterMemory(ctx context.Context, world, character string, memoryBody, chronicleTail []byte) ([]byte, error)
}

// LoreMaintainThreshold is the line count at which
// MaintainLore asks the LLM-driven summarizer to compact
// a world's lore.md. 500 lines is roughly 60-80
// deviation entries — generous enough that a long
// campaign does not hit it on day 5, tight enough that
// the file does not eat the entire context_window by
// day 50. Operators with a smaller / larger lore
// corpus can override in code (the threshold is a const
// in this package, not a YAML field — moving it to
// config is a one-line follow-up if anyone asks).
const LoreMaintainThreshold = 500

// CharacterMemoryMaintainBytes is the file size
// (in bytes) at which MaintainCharacterMemory
// asks the LLM-driven summarizer to defragment
// the active character's memory.yaml. Below the
// threshold the file is left untouched — running
// a LLM call on a 2KB file would burn budget
// without a meaningful shrink.
//
// 4KB is roughly 17 sections of average-length
// bullets (the markus migration produced 30KB
// from a long campaign; per-day play would
// reach 4KB around day 10–15). The threshold is
// intentionally file-size-based (not section
// count) because the cost driver is "how much
// goes into the prompt every turn" not "how
// many sections the model has to scan".
const CharacterMemoryMaintainBytes = 4096
