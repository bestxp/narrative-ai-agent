// Package prompts: data-bag (PromptData) that aggregates
// everything a prompt template might need. Templates
// receive a single root object and reach into typed
// sub-structs. There is no cross-package reach into
// domain.* / config.* / npcprofile.* — all the projection
// happens here in NewPromptData, so templates stay
// declarative and decoupled.
//
// The shared tunable thresholds (NPCPersonalMemoryLimit,
// StageRenderMaxBytes, ...) live in internal/limits
// and are referenced by name. This package does NOT
// carry its own copy — that path led to drift and
// required a unit test pinning the two values
// together. The single source of truth is now
// internal/limits, and this package just forwards
// the values into the data-bag.
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
	// LegacyNPC carries the legacy tools.NPCProfile
	// fields (free-text scalars) for the
	// npc_profile_legacy.md.tmpl template. Exactly one
	// of NPCProfile / LegacyNPC is populated per render.
	LegacyNPC *LegacyNPCProfileData
	State     *StateData
}

// NarrativeData is the subset of config.NarrativeConfig
// that templates can reference. Names match the YAML
// field names (snake_case → CamelCase) so the data
// feels "config-shaped" to a template author.
type NarrativeData struct {
	WordLimit                  int
	Language                   string
	RulesCheckBlock            bool
	IncludeSystemStateInPrompt bool
	CompactionNotify           bool
	CompactionNotifyVerbose    bool
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
}

// DisplayData covers on-the-wire / UI constants. They
// affect how the LLM is told to format its output
// (e.g. JSON shape, language hints, soft caps).
type DisplayData struct {
	Language           string
	SystemStateSummary string
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

// LegacyNPCProfileData is the structured shape used by
// npc_profile_legacy.md.tmpl. It mirrors the legacy
// tools.NPCProfile struct (free-text scalar fields)
// and is kept separate from NPCProfileData so the
// modern npcprofile.Profile (with strict typed arrays)
// is the canonical path. The legacy renderer stays
// here for the few callers that still hand the
// toolset a tools.NPCProfile directly (tests, the
// /me command, and any external integration).
type LegacyNPCProfileData struct {
	DisplayName       string
	Nicknames         []string
	Temperament       string
	Relations         string
	Abilities         []string
	PersonalMemory    string
	CurrentStatus     string
	CriticalKnowledge string
	LastUpdate        string
}

// NPCRelationRow is one bullet under "## Отношения с
// другими NPC". Target and Note are pre-trimmed strings.
type NPCRelationRow struct {
	Target string
	Note   string
}

// StateData is the structured shape used by
// state.md.tmpl (replaces domain.BuildStateMarkdown).
type StateData struct {
	World    string
	Day      int
	InFlight bool
	Location string
	NPCs     []string
	Moment   string
	Events   []string
}

// NarrativeConfigSnapshot is the projection of the
// operator's config.yaml into the narrative knobs
// templates need. Callers build this from
// *config.NarrativeConfig + *config.CompactionConfig
// in main.go; tests build it directly.
type NarrativeConfigSnapshot struct {
	WordLimit                  int
	Language                   string
	RulesCheckBlock            bool
	IncludeSystemStateInPrompt bool
	CompactionNotify           bool
	CompactionNotifyVerbose    bool

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
			WordLimit:                  narrative.WordLimit,
			Language:                   narrative.Language,
			RulesCheckBlock:            narrative.RulesCheckBlock,
			IncludeSystemStateInPrompt: narrative.IncludeSystemStateInPrompt,
			CompactionNotify:           narrative.CompactionNotify,
			CompactionNotifyVerbose:    narrative.CompactionNotifyVerbose,
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
	location, moment string,
	npcs, events []string,
) *StateData {
	return &StateData{
		World:    world,
		Day:      day,
		InFlight: inFlight,
		Location: location,
		NPCs:     append([]string(nil), npcs...),
		Moment:   moment,
		Events:   append([]string(nil), events...),
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

func pickInt(got, def int) int {
	if got > 0 {
		return got
	}
	return def
}
