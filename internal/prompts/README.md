# `internal/prompts` — LLM-facing templates

All LLM-facing text in the bot lives here. The
files are embedded in the binary via `//go:embed` and
rendered through `text/template` with a single
data-bag (`PromptData`). The same compile-time
discipline that guards `.go` files guards these
files: a typo in a `{{ .Field }}` marker fails at
startup, not silently at runtime.

## Templates

| File | Role | Rendered by |
|------|------|-------------|
| `narrative.md.tmpl` | system prompt (rules only) | `cmd/bot/main.go` once at startup + on `/reload` |
| `world_state.md.tmpl` | user[0] — `[WORLD_STATE]` | `domain.BuildWorldStateMessage` on every turn |
| `state.md.tmpl` | `worlds/<w>/state.md` body | `usecase/tools/files/state.go:UpdateState` |
| `npc_profile.md.tmpl` | `characters/<dir>/<slug>.yaml` markdown view (canonical) | `npcprofile.Profile.BuildMarkdown` |
| `compaction_in_place.md.tmpl` | in-place compaction prompt | `cmd/bot/main.go` once at startup |
| `end_of_day.md.tmpl` | end-of-day protocol prompt | same |
| `character_memory_maintain.md.tmpl` | defrag character memory | same |
| `npc_summary.md.tmpl` | defrag NPC profiles | same |
| `lore_summary.md.tmpl` | defrag lore.md | same |
| `chronicle_summary.md.tmpl` | chronicle window compressor system prompt | `usecase.Summarizer.SetChronicleSummaryPrompt` once at startup |
| `summarizer_old_turns_user.md.tmpl` | OldTurns user message | `usecase.Summarizer.SummarizeOldTurns` per call |
| `summarizer_inplace_user.md.tmpl` | InPlace user message | `usecase.Summarizer.SummarizeInPlace` per call |
| `summarizer_eod_user.md.tmpl` | EndOfDay user message | `usecase.Summarizer.SummarizeEndOfDay` per call |
| `summarizer_npc_user.md.tmpl` | NPC user message | `usecase.Summarizer.SummarizeNPC` per call |
| `summarizer_lore_user.md.tmpl` | Lore user message | `usecase.Summarizer.SummarizeLore` per call |
| `summarizer_chronicle_user.md.tmpl` | Chronicle user message | `usecase.Summarizer.SummarizeChronicle` per call |
| `summarizer_charmem_user.md.tmpl` | CharacterMemory user message | `usecase.Summarizer.SummarizeCharacterMemory` per call |

## Data-bag

Every template receives the same root object:

```go
type PromptData struct {
    Narrative  NarrativeData
    Compaction CompactionData
    Display    DisplayData
    Character  CharacterData
    World      WorldData
    NPCProfile *NPCProfileData    // NPC files only
    State      *StateData         // state.md only
}
```

Templates that do not need a sub-struct leave it
zero. `text/template` skips empty blocks; the
template author decides order and conditional
rendering, not Go.

## Adding a new field

1. Add the field to the matching `*Data` struct in
   `data.go`.
2. Populate it from the caller (the existing
   `NewPromptData` aggregate in `data.go` covers the
   common case; per-template helpers
   `NewStateData` / `NewNPCProfileDataFromFields`
   cover the rest).
3. Reference it from the template as
   `{{ .SubStruct.Field }}`.
4. Add a test in `prompts_test.go` that exercises
   the new field end-to-end through `Render`.

A typo in the template (`{{ .Narrtive.WordLimit }}`
instead of `{{ .Narrative.WordLimit }}`) fails
immediately at `Render` time — `missingkey=error`
is set on the template parse options, so a silent
`<no value>` is impossible.

## Override-on-disk (operator escape hatch)

`LoadSystemPrompt(overridePath, defaultName)` still
works for callers that want a hand-written override
on disk. The override is read verbatim and rendered
through the same data-bag (override files are
treated as templates too, with the same
`missingkey=error` discipline).

No hot-reload, no `fsnotify`. The override is
re-read only on process restart. Power-users can
use `/reload` to flush the in-memory snapshot
cache, but the templates themselves are bound at
startup.

## Adding a new template

1. Drop `<name>.md.tmpl` in this directory.
   `//go:embed *.md.tmpl` picks it up automatically.
2. Add a `Render("<name>.md.tmpl", data)` callsite
   in the right caller (system / world / state /
   npc / summarizer).
3. Add a unit test that asserts the rendered
   output contains the markers you care about.

Templates live next to `.go` code because they are
code, not data. There is no override-on-disk for
the template files themselves; an operator
experimenting with a prompt variant does so on a
separate branch and ships a new binary.

## Summarizer user-message templates

The 7 summarizer methods in `internal/usecase/summarizer.go`
template their **system prompt** through `Render` (one
render at startup via `renderSummarizerPrompt`), and
template their **user message** through the `Summarizer.renderSummary`
helper (per-call) using the templates below. The Go side
passes **only structured data** (`*NPCProfileData`,
`*ChronicleData`, `*StateData`, `[]MessageData`) — never
pre-rendered strings. The templates own ALL formatting: YAML
emission from struct fields, markdown blocks, code fences,
day padding, role labels, instruction lines. `projectMessages`,
`projectChronicle`, `projectNPCProfile`, `projectState` are
data-only projections (allowed on Go side — they produce
data, not LLM-facing text).

7 typed constructors replace the old 12-arg `NewSummarizerData`:
`NewOldTurnsSummaryData`, `NewInPlaceSummaryData`,
`NewEndOfDaySummaryData`, `NewNPCSummaryData`,
`NewLoreSummaryData`, `NewChronicleSummaryData`,
`NewCharacterMemorySummaryData`. Each takes only the fields
its method needs.

| File | Method | Struct fields used |
|------|--------|--------------------|
| `summarizer_old_turns_user.md.tmpl` | `SummarizeOldTurns` | `Messages` |
| `summarizer_inplace_user.md.tmpl` | `SummarizeInPlace` | `World`, `Day`, `Messages` |
| `summarizer_eod_user.md.tmpl` | `SummarizeEndOfDay` | `World`, `Day`, `Messages`, `State` |
| `summarizer_npc_user.md.tmpl` | `SummarizeNPC` | `World`, `NPCDisplayName`, `NPCProfile`, `Chronicle` |
| `summarizer_lore_user.md.tmpl` | `SummarizeLore` | `World`, `LoreBody`, `Chronicle`, `State` |
| `summarizer_chronicle_user.md.tmpl` | `SummarizeChronicle` | `World`, `StartDay`, `EndDay`, `Chronicle` |
| `summarizer_charmem_user.md.tmpl` | `SummarizeCharacterMemory` | `World`, `CharacterDisplayName`, `CharacterMemoryBody`, `Chronicle` |

`LoreBody` and `CharacterMemoryBody` stay as raw strings
because there is no ready-made structured type for those
file formats (lore.md is markdown with `## header` +
`- bullet`; memory.yaml is `data: [{section, values}]`
emitted back as YAML by the LLM). When a dedicated
structure is added later these can become pointers too.

The summarizer user templates also reference
`{{ .Compaction.* }}` knobs (InPlaceSummaryWordsMin/Max,
EndOfDaySummaryWords*, OldTurnsSummaryTokens*,
LoreTargetLines*, MemoriseSentence*) — these are wired via
`Summarizer.SetCompactionConfig` at startup, so the
templates never hard-code magic numbers.
