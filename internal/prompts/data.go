// Package prompts serves the bundled skill prompts. The source
// files live in this directory; they are baked into the
// binary at build time via //go:embed so the operator does not
// have to ship them next to the executable.
//
// Why embed instead of a relative path at runtime:
//
//   - The skill files are *behaviour* of the bot, not data.
//     A typo or a missing file would change how the GM
//     reasons, silently, and the operator would have no clue
//     which version of the prompt is actually in use.
//   - Single-file deploys: `bot-windows-amd64.exe config.yaml`
//     is enough. No need to remember to copy the prompts dir.
//
// If a future role needs its own prompt, add the .md.tmpl
// file to this directory and call
// `prompts.Render("<name>.md.tmpl", data)` — go:embed will
// pick it up automatically thanks to the wildcard.
//
// Templates are *code*, not data — same lifecycle as .go:
// a typo = a build-time error, never a silent runtime drift.
//
// The data-bag (PromptData) aggregates everything a template
// might need. Templates receive a single root object and
// reach into typed sub-structs. There is no cross-package
// reach into domain.* / config.* / npcprofile.* — all the
// projection happens here in NewPromptData, so templates
// stay declarative and decoupled.
//
// The shared tunable thresholds (NPCPersonalMemoryLimit,
// StageRenderMaxBytes, ...) live in internal/limits and are
// referenced by name. This package does NOT carry its own
// copy — that path led to drift and required a unit test
// pinning the two values together. The single source of
// truth is now internal/limits, and this package just
// forwards the values into the data-bag.
package prompts

import "github.com/bestxp/narrative-ai-agent/internal/limits"

// Defaults for the CompactionData sub-struct. These are
// forwarded from internal/limits (the single source of
// truth) so a single edit propagates to the LLM
// template, the auto-deflate logic in npcprofile, and
// the staging renderer. The `promptpkg.Default*` names
// stay here as thin aliases for backward compatibility
// with callers that pre-date the limits refactor.
const (
	DefaultNPCPersonalMemoryLimit     = limits.NPCPersonalMemoryLimit
	DefaultNPCPersonalMemoryTarget    = limits.NPCPersonalMemoryTarget
	DefaultMemoryTargetBytes          = limits.MemoryTargetBytes
	DefaultLoreLineLimit              = limits.LoreLineLimit
	DefaultLoreTargetLines            = limits.LoreTargetLines
	DefaultMemoriseWindowDays         = limits.MemoriseWindowDays
	DefaultMemoriseSentencesPer30Days = limits.MemoriseSentencesPer30Days
	DefaultStageRenderMaxBytes        = limits.StageRenderMaxBytes
	DefaultProtocolWindowDays         = limits.ProtocolWindowDays
	DefaultProtocolMaxChars           = limits.ProtocolMaxChars
	DefaultInPlaceSummaryWordsMin     = limits.InPlaceSummaryWordsMin
	DefaultInPlaceSummaryWordsMax     = limits.InPlaceSummaryWordsMax
	DefaultEndOfDaySummaryWordsMin    = limits.EndOfDaySummaryWordsMin
	DefaultEndOfDaySummaryWordsMax    = limits.EndOfDaySummaryWordsMax
	DefaultOldTurnsSummaryTokensMin   = limits.OldTurnsSummaryTokensMin
	DefaultOldTurnsSummaryTokensMax   = limits.OldTurnsSummaryTokensMax
	DefaultLoreTargetLinesMin         = limits.LoreTargetLinesMin
	DefaultLoreTargetLinesMax         = limits.LoreTargetLinesMax
	DefaultLoreSectionTargetMin       = limits.LoreSectionTargetMin
	DefaultLoreSectionTargetMax       = limits.LoreSectionTargetMax
	DefaultMemoriseSentenceMin        = limits.MemoriseSentenceMin
	DefaultMemoriseSentenceMax        = limits.MemoriseSentenceMax
)

// PromptData is the root data structure handed to every
// prompt template. Sections are deliberately flat: each
// domain gets its own sub-struct so a template author can
// reach e.g. {{ .Narrative.WordLimit }} without having to
// know which file in the bot generates the value.
type PromptData struct {
	Narrative  NarrativeData
	Compaction CompactionData
	Display    DisplayData
	Character  CharacterData
	World      WorldData
	NPCProfile *NPCProfileData
	State      *StateData
	// Summarizer carries the raw inputs for a summarizer
	// user-message render. Populated only by the 7
	// Summarizer.Summarize* methods; nil for all other
	// templates. The sub-struct holds raw data only —
	// every LLM-facing string (headers, fences,
	// instructions, day formatting, message roles) is
	// rendered by the template, never by Go.
	Summarizer *SummarizerData
}

// NarrativeData is the subset of config.NarrativeConfig
// that templates can reference. Names match the YAML
// field names (snake_case → CamelCase) so the data
// feels "config-shaped" to a template author.
type NarrativeData struct {
	WordLimit               int
	WordLimitFloor          int
	Language                string
	RulesCheckBlock         bool
	CompactionNotify        bool
	CompactionNotifyVerbose bool
}

// CompactionData exposes the hard-coded compaction
// thresholds and content limits that templates need to
// reference (e.g. memory-target, memorise-window).
// Sources of truth: npcprofile.NPCPersonalMemoryLimit,
// staging.MaxStageRenderBytes, internal/state md
// constants.
type CompactionData struct {
	NPCPersonalMemoryLimit     int
	NPCPersonalMemoryTarget    int
	MemoryTargetBytes          int
	LoreLineLimit              int
	LoreTargetLines            int
	MemoriseWindowDays         int
	MemoriseSentencesPer30Days int
	StageRenderMaxBytes        int
	ProtocolWindowDays         int
	ProtocolMaxChars           int
	// In-place compaction word-count range (soft target).
	InPlaceSummaryWordsMin int
	InPlaceSummaryWordsMax int
	// End-of-day protocol word-count range (soft target).
	EndOfDaySummaryWordsMin int
	EndOfDaySummaryWordsMax int
	// Old-turns compaction token-count range (soft target).
	OldTurnsSummaryTokensMin int
	OldTurnsSummaryTokensMax int
	// Lore-summary line-count band (around LoreTargetLines).
	LoreTargetLinesMin int
	LoreTargetLinesMax int
	// Lore-summary section-count band ("примерно N-M секций").
	LoreSectionTargetMin int
	LoreSectionTargetMax int
	// Memorise-summary acceptable sentence band around
	// MemoriseSentencesPer30Days.
	MemoriseSentenceMin int
	MemoriseSentenceMax int
}

// DisplayData covers on-the-wire / UI constants. They
// affect how the LLM is told to format its output
// (e.g. JSON shape, language hints, soft caps).
type DisplayData struct {
	Language string
}

// CharacterData is the per-turn dynamic character
// context, a thin wrapper over domain.CharacterContext
// so templates don't reach into domain/ packages.
// Fields are pre-rendered strings (no nested templates).
type CharacterData struct {
	Name   string
	SOUL   string
	SKILL  string
	Memory string
}

// WorldData wraps domain.WorldContext with the same
// intent: it carries the snapshot of the active world
// that the [WORLD_STATE] block needs.
type WorldData struct {
	Name  string
	State string
	Canon string
	Lore  string
	Plan  string
	// Chronicle is the LLM-compressed window log
	// (Periods) plus the raw per-day log for the open
	// (unclosed) window (Days). Rendered into the
	// "Воспоминания за периоды" and "Последняя
	// хронология событий" blocks of user[0]. Nil →
	// both blocks hidden (a brand-new world).
	Chronicle   *ChronicleData
	Stage       string
	NPCRegistry string
	ActiveNPCs  []NPCSnapshotData
}

// ChronicleData is the rendered shape of one world's
// chronicle: a list of LLM-compressed windows
// (Periods) plus the raw per-day log for the open
// window (Days). Both fields are slices so the
// renderer can use {{ range }} directly.
//
// Mirrors chronicle.Chronicle; the prompts package
// does not import the chronicle package to avoid an
// import cycle (usecase → prompts → chronicle vs
// usecase → chronicle → prompts).
type ChronicleData struct {
	Periods []ChroniclePeriodData
	Days    []ChronicleDayData
}

// ChroniclePeriodData is one LLM-compressed window
// covering raw days [From..To].
type ChroniclePeriodData struct {
	From   int
	To     int
	Memory string
}

// ChronicleDayData is one raw per-day entry.
type ChronicleDayData struct {
	Number int
	Text   string
}

// NPCSnapshotData mirrors domain.NPCSnapshot for the
// template layer; same render-ready strings.
type NPCSnapshotData struct {
	DisplayName string
	Profile     string
}

// NPCProfileData is the structured shape used by
// npc_profile.md.tmpl (replaces the old Go-side
// Profile.BuildMarkdown). Optional: only NPC-specific
// templates populate it.
type NPCProfileData struct {
	DisplayName       string
	Temperament       string
	RelationsGG       string
	RelationsNPCs     []NPCRelationRow
	Abilities         []string
	PersonalMemory    []string
	CurrentStatus     string
	CriticalKnowledge []string
	Nicknames         []string
	LastUpdate        string
}

// NPCRelationRow is one bullet under "## Отношения с
// другими NPC". Target and Note are pre-trimmed strings.
type NPCRelationRow struct {
	Target string
	Note   string
}

// StateData is the structured shape used by the YAML
// round-trip in world_state_yaml.go (planning/0001:
// state.md + stage.md → state.yaml). The on-disk
// format is the canonical example at
// running/game-data/worlds/naruto/state.yaml. There is
// no template — the encoder is plain yaml.v3, not a
// text template. StateData exists only so tests and
// non-encoder code paths can hold a typed projection
// of StateSnapshot without dragging the yaml.v3 types
// across packages.
//
// Every field mirrors StateSnapshot. Optional
// fields (omitempty) match the wire shape; strings
// like Moment / Daytime / Location / Current always
// appear in the file.
type StateData struct {
	World    string
	Day      int
	InFlight bool
	Daytime  string
	Location string
	Moment   string
	NPCs     []string
	Current  string
	Events   []string
	// Stage is the runtime-only slice of the plot
	// graph. Same shape as domain.StageState; mirrored
	// here to keep prompts free of staging imports.
	// Value, not pointer — the stage block is a
	// permanent part of state.yaml from the first turn.
	Stage StageStateData
}

// StageStateData mirrors domain.StageState — three
// primitives (current / timeline_index / next).
type StageStateData struct {
	Current       string
	TimelineIndex int
	Next          string
}

// NarrativeConfigSnapshot is the projection of the
// operator's config.yaml into the narrative knobs
// templates need. Callers build this from
// *config.NarrativeConfig + *config.CompactionConfig
// in main.go; tests build it directly.
type NarrativeConfigSnapshot struct {
	WordLimit               int
	Language                string
	RulesCheckBlock         bool
	CompactionNotify        bool
	CompactionNotifyVerbose bool

	// Compaction knobs (zero = use Go-side default).
	NPCPersonalMemoryLimit     int
	NPCPersonalMemoryTarget    int
	MemoryTargetBytes          int
	LoreLineLimit              int
	LoreTargetLines            int
	MemoriseWindowDays         int
	MemoriseSentencesPer30Days int
	StageRenderMaxBytes        int
	ProtocolWindowDays         int
	ProtocolMaxChars           int
}

// NewPromptData builds the root data-bag from a config
// snapshot + per-turn context. Called once per LLM
// request; cheap (struct copy + string passthrough).
//
// To keep this package free of cross-domain imports
// (and to break the would-be import cycle with
// internal/domain, which calls back into prompts.Render
// for the world-state template), the function takes
// primitive types only. Callers project their own
// domain.* types into the data-bag in their own code
// (see gm.go).
func NewPromptData(
	narrative NarrativeConfigSnapshot,
	character CharacterData,
	world WorldData,
) PromptData {
	return PromptData{
		Narrative: NarrativeData{
			WordLimit:               narrative.WordLimit,
			WordLimitFloor:          limits.NarrativeWordLimitFloor,
			Language:                narrative.Language,
			RulesCheckBlock:         narrative.RulesCheckBlock,
			CompactionNotify:        narrative.CompactionNotify,
			CompactionNotifyVerbose: narrative.CompactionNotifyVerbose,
		},
		Compaction: CompactionData{
			NPCPersonalMemoryLimit:     pickInt(narrative.NPCPersonalMemoryLimit, DefaultNPCPersonalMemoryLimit),
			NPCPersonalMemoryTarget:    pickInt(narrative.NPCPersonalMemoryTarget, DefaultNPCPersonalMemoryTarget),
			MemoryTargetBytes:          pickInt(narrative.MemoryTargetBytes, DefaultMemoryTargetBytes),
			LoreLineLimit:              pickInt(narrative.LoreLineLimit, DefaultLoreLineLimit),
			LoreTargetLines:            pickInt(narrative.LoreTargetLines, DefaultLoreTargetLines),
			MemoriseWindowDays:         pickInt(narrative.MemoriseWindowDays, DefaultMemoriseWindowDays),
			MemoriseSentencesPer30Days: pickInt(narrative.MemoriseSentencesPer30Days, DefaultMemoriseSentencesPer30Days),
			StageRenderMaxBytes:        pickInt(narrative.StageRenderMaxBytes, DefaultStageRenderMaxBytes),
			ProtocolWindowDays:         pickInt(narrative.ProtocolWindowDays, DefaultProtocolWindowDays),
			ProtocolMaxChars:           pickInt(narrative.ProtocolMaxChars, DefaultProtocolMaxChars),
			InPlaceSummaryWordsMin:     DefaultInPlaceSummaryWordsMin,
			InPlaceSummaryWordsMax:     DefaultInPlaceSummaryWordsMax,
			EndOfDaySummaryWordsMin:    DefaultEndOfDaySummaryWordsMin,
			EndOfDaySummaryWordsMax:    DefaultEndOfDaySummaryWordsMax,
			OldTurnsSummaryTokensMin:   DefaultOldTurnsSummaryTokensMin,
			OldTurnsSummaryTokensMax:   DefaultOldTurnsSummaryTokensMax,
			LoreTargetLinesMin:         DefaultLoreTargetLinesMin,
			LoreTargetLinesMax:         DefaultLoreTargetLinesMax,
			LoreSectionTargetMin:       DefaultLoreSectionTargetMin,
			LoreSectionTargetMax:       DefaultLoreSectionTargetMax,
			MemoriseSentenceMin:        DefaultMemoriseSentenceMin,
			MemoriseSentenceMax:        DefaultMemoriseSentenceMax,
		},
		Display: DisplayData{
			Language: narrative.Language,
		},
		Character: character,
		World:     world,
	}
}

// NewStateData projects a domain-free state snapshot
// (used by state.md.tmpl) into the data-bag. The caller
// (usecase/tools/files/state.go) projects its own
// StateSnapshot into the data-bag in its own code to
// avoid an import cycle with internal/domain.
func NewStateData(
	world string, day int, inFlight bool,
	daytime, location, moment, current string,
	stage StageStateData,
	npcs, events []string,
) *StateData {
	return &StateData{
		World:    world,
		Day:      day,
		InFlight: inFlight,
		Daytime:  daytime,
		NPCs:     append([]string(nil), npcs...),
		Current:  current,
		Location: location,
		Moment:   moment,
		Events:   append([]string(nil), events...),
		Stage:    stage,
	}
}

// NewNPCProfileDataFromFields is the public entry used
// by writer-tool code. It takes already-projected
// strings/slices, so this package does not need to
// import npcprofile.
func NewNPCProfileDataFromFields(
	displayName, temperament, relationsGG string,
	relationsNPCs []NPCRelationRow,
	abilities, personalMemory, criticalKnowledge, nicknames []string,
	currentStatus, lastUpdate string,
) *NPCProfileData {
	return &NPCProfileData{
		DisplayName:       displayName,
		Temperament:       temperament,
		RelationsGG:       relationsGG,
		RelationsNPCs:     relationsNPCs,
		Abilities:         abilities,
		PersonalMemory:    personalMemory,
		CurrentStatus:     currentStatus,
		CriticalKnowledge: criticalKnowledge,
		Nicknames:         nicknames,
		LastUpdate:        lastUpdate,
	}
}

// ConvertNPCSnapshots projects a slice of
// (DisplayName, Profile) pairs into NPCSnapshotData.
// Callers in gm.go use this to project their
// domain.NPCSnapshot into the template-friendly shape.
func ConvertNPCSnapshots(displayName, profile []string) []NPCSnapshotData {
	n := len(displayName)
	if n != len(profile) {
		return nil
	}

	out := make([]NPCSnapshotData, n)
	for i := range displayName {
		out[i] = NPCSnapshotData{DisplayName: displayName[i], Profile: profile[i]}
	}

	return out
}

// SummarizerData carries the raw, structural inputs that
// a summarizer user-message template renders. Fields are
// either primitive scalars (World, Day, StartDay, EndDay,
// display names) or pointer-to-struct (NPCProfile,
// Chronicle, State) — NEVER pre-rendered LLM-facing strings.
// The template owns ALL formatting: headers, code fences,
// day padding, YAML emission from NPCProfile fields, role
// labels for Messages, and the final instruction line. The
// Go side never touches LLM-facing text.
//
// LoreBody and CharacterMemoryBody stay as raw strings
// because there is no ready-made structured type for
// those file formats (lore.md is markdown with `## header`
// + `- bullet` lines; memory.yaml is `data: [{section,
// values}]` which the LLM emits back as YAML). When a
// dedicated structure is added later these two fields
// can become pointers too.
//
// Not every field is populated on every call. Each of the
// 7 constructors (NewOldTurnsSummaryData etc.) fills only
// the fields its method needs; the template uses {{ if }}
// guards to skip nil pointers and empty values.
type SummarizerData struct {
	World    string
	Day      int
	StartDay int
	EndDay   int
	// NPCDisplayName is set for SummarizeNPC.
	NPCDisplayName string
	// CharacterDisplayName is set for
	// SummarizeCharacterMemory.
	CharacterDisplayName string
	// NPCProfile is the structured NPC profile for
	// SummarizeNPC. The template renders the YAML body
	// from this struct via {{ range .NPCProfile.* }}.
	// Nil for all other methods.
	NPCProfile *NPCProfileData
	// LoreBody is the raw markdown body for
	// SummarizeLore. String (not a struct) because
	// lore.md has no dedicated structured type.
	LoreBody string
	// CharacterMemoryBody is the raw YAML body for
	// SummarizeCharacterMemory. String (not a struct)
	// because memory.yaml's `data: [{section, values}]`
	// shape is emitted back by the LLM as YAML; we feed
	// it verbatim inside a fence.
	CharacterMemoryBody string
	// Chronicle is the structured chronicle (periods +
	// days) used as context by NPC, Lore and
	// CharacterMemory calls, and as the full body by
	// SummarizeChronicle. Nil → the template's
	// {{ if .Chronicle }} blocks emit nothing.
	Chronicle *ChronicleData
	// State is the structured world state for
	// SummarizeEndOfDay and SummarizeLore. Nil →
	// template skips the state block.
	State *StateData
	// Messages is the conversation turns for OldTurns,
	// InPlace and EndOfDay. Empty slice (or nil) → the
	// template's {{ range }} emits nothing.
	Messages []MessageData
}

// MessageData is the template-facing shape of one
// llm.Message. The projection from llm.Message to
// MessageData happens in the Summarizer (it's data, not
// LLM-facing text — Go-side projection is allowed). The
// template decides how to render each role: [Игрок]: for
// user, [GM]: for assistant, [→ <name>]: for tool. Empty
// assistant bodies with ToolCalls are rendered as
// "(вызвал tools: a,b)" by the template via {{ if .ToolCalls
// }}.
type MessageData struct {
	Role      string
	Content   string
	Name      string
	ToolCalls []string
}

// NewOldTurnsSummaryData builds the data-bag for
// SummarizeOldTurns: only the conversation messages.
func NewOldTurnsSummaryData(messages []MessageData) *SummarizerData {
	return &SummarizerData{Messages: messages}
}

// NewInPlaceSummaryData builds the data-bag for
// SummarizeInPlace: world, current day, conversation.
func NewInPlaceSummaryData(world string, day int, messages []MessageData) *SummarizerData {
	return &SummarizerData{World: world, Day: day, Messages: messages}
}

// NewEndOfDaySummaryData builds the data-bag for
// SummarizeEndOfDay: world, closing day, conversation and
// the structured world state at close time. state may be
// nil — the template skips the state block.
func NewEndOfDaySummaryData(world string, day int, messages []MessageData, state *StateData) *SummarizerData {
	return &SummarizerData{World: world, Day: day, Messages: messages, State: state}
}

// NewNPCSummaryData builds the data-bag for SummarizeNPC:
// world, NPC display name, structured NPC profile, and the
// chronicle window as context. chronicle may be nil.
func NewNPCSummaryData(world, displayName string, profile *NPCProfileData, chronicle *ChronicleData) *SummarizerData {
	return &SummarizerData{World: world, NPCDisplayName: displayName, NPCProfile: profile, Chronicle: chronicle}
}

// NewLoreSummaryData builds the data-bag for SummarizeLore:
// world, raw lore.md body, chronicle window and the
// structured world state as context. chronicle / state
// may be nil.
func NewLoreSummaryData(world, loreBody string, chronicle *ChronicleData, state *StateData) *SummarizerData {
	return &SummarizerData{World: world, LoreBody: loreBody, Chronicle: chronicle, State: state}
}

// NewChronicleSummaryData builds the data-bag for
// SummarizeChronicle: world, the day window being
// collapsed, and the whole structured chronicle (periods
// + days) as context for cross-window dedup.
func NewChronicleSummaryData(world string, startDay, endDay int, chronicle *ChronicleData) *SummarizerData {
	return &SummarizerData{World: world, StartDay: startDay, EndDay: endDay, Chronicle: chronicle}
}

// NewCharacterMemorySummaryData builds the data-bag for
// SummarizeCharacterMemory: world, character display name,
// the raw memory.yaml body, and the chronicle window as
// context. chronicle may be nil.
func NewCharacterMemorySummaryData(world, character, memoryBody string, chronicle *ChronicleData) *SummarizerData {
	return &SummarizerData{World: world, CharacterDisplayName: character, CharacterMemoryBody: memoryBody, Chronicle: chronicle}
}

func pickInt(got, def int) int {
	if got > 0 {
		return got
	}

	return def
}
