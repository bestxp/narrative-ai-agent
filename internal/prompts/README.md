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
| `npc_profile_legacy.md.tmpl` | legacy `tools.NPCProfile` markdown view | (kept for back-compat with tests) |
| `compaction_in_place.md.tmpl` | in-place compaction prompt | `cmd/bot/main.go` once at startup |
| `end_of_day.md.tmpl` | end-of-day protocol prompt | same |
| `character_memory_maintain.md.tmpl` | defrag character memory | same |
| `npc_summary.md.tmpl` | defrag NPC profiles | same |
| `lore_summary.md.tmpl` | defrag lore.md | same |
| `memorise_summary.md.tmpl` | window compressor | same |

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
    LegacyNPC  *LegacyNPCProfileData // legacy NPC struct only
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
