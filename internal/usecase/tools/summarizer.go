package tools

import "context"

// NPCSummarizer is the LLM-driven compaction hook the
// file-backed NPC maintainer calls when a profile's
// personal_memory list has grown past the threshold
// (npcprofile.NPCPersonalMemoryLimit = 40). It lives in
// the tools package as a small, focused interface so
// the file implementation (files.Memory) can be wired
// against any summarizer — the production usecase-based
// one in usecase.Summarizer, or a stub in tests.
//
// The summarizer is given a display name (so the LLM
// can address the NPC in its system prompt), the
// current profile body in YAML form, and the world's
// memorise.md tail for context. It returns a NEW YAML
// body (or the original, if it could not compress
// further). The contract is:
//
//   1. The returned body MUST be parseable as a
//      npcprofile.Profile (YAML). If the LLM emits
//      invalid YAML the caller logs a warning and
//      leaves the original file untouched.
//   2. The summarizer MUST NOT call back into the
//      tools layer (no AppendLore, no UpdateNPC). It
//      receives the world name + memorise.md tail
//      purely as read context, not as a write handle.
//   3. The summarizer is best-effort. It may decide
//      the profile is already tight (returns the
//      input unchanged) — callers must not assume
//      every call produces a smaller body.
//   4. The summarizer is invoked synchronously by the
//      maintenance tool. The implementation may
//      respect ctx cancellation.
type NPCSummarizer interface {
	SummarizeNPC(ctx context.Context, displayName, world string, yamlBody, memoriseTail []byte) ([]byte, error)
}

// LoreSummarizer is the LLM-driven compaction hook
// for the file-backed lore maintainer. It is invoked
// when a world's lore.md grows past LoreMaintainThreshold
// (500 lines by default). The contract mirrors
// NPCSummarizer:
//
//   1. The returned body MUST be the same markdown
//      format the dispatcher expects ("## header"
//      sections + "- bullet" lines). If the LLM emits
//      something else the caller logs a warning and
//      leaves the file alone.
//   2. The summarizer MUST NOT call back into the
//      tools layer. It receives memorise.md tail and
//      state.md for read-only context, no write handle.
//   3. Best-effort: returns the input unchanged when
//      the summarizer could not compress.
//   4. Respects ctx cancellation.
type LoreSummarizer interface {
	SummarizeLore(ctx context.Context, world string, loreBody, memoriseTail, stateMD []byte) ([]byte, error)
}

// MemoriseSummarizer is the LLM-driven compaction hook for the
// file-backed memorise maintainer. It is invoked automatically
// after ArchiveDay records a day that closes a window, and on
// any larger timeskip (the caller computes the actual day range
// to compact, not a fixed 30).
//
// The summarizer receives the world name, the day range being
// collapsed (start, end, both inclusive), and the WHOLE current
// memorise.md file as read context. The whole-file context is
// important: the model needs the earlier, already-compressed
// summaries to keep its prose consistent (it must not invent
// facts that contradict an earlier window, and it should
// dedupe any cross-window repetitions such as a 15-day
// training arc that spans the boundary).
//
// The contract mirrors NPCSummarizer / LoreSummarizer:
//
//  1. The returned body MUST start with the literal prefix
//     "д<start>-д<end>: " (5-digit zero-padded day numbers,
//     same shape as the day-line entries). The caller appends
//     it to the file as a single line; malformed output is
//     rejected by the caller (file is left untouched).
//  2. The summarizer MUST NOT call back into the tools
//     layer. It is read-only over the supplied context.
//  3. Best-effort: the model may decide the window is already
//     too thin to compress (e.g. only 5 days of real activity
//     in a 30-day window) and return an empty body — the caller
//     treats that as "no compression happened" and skips.
//  4. The summarizer SHOULD aim for ~10 sentences for a
//     30-day window, scaling roughly linearly for wider
//     windows (60 days → ~20 sentences, 90 days → ~30). The
//     exact target is in the system prompt.
//  5. Respects ctx cancellation.
type MemoriseSummarizer interface {
	SummarizeMemorise(ctx context.Context, world string, startDay, endDay int, fullMemorise string) ([]byte, error)
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
