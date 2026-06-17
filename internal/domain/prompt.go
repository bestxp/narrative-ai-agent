package domain

import (
	"strings"

	"github.com/bestxp/narrative-ai-agent/internal/prompts"
)

// CharacterContext is the per-turn bundle of player-character
// static data: SOUL/SKILL/memory files and the list of available
// `## <section>` headers for `update_character`. The character
// is the SAME across every world and scene, so it goes into
// the system prompt (Индекс 0) — the part of the payload that
// is stable for the entire conversation.
//
// Character data used to be embedded into BuildSystemPrompt
// alongside world state. Splitting them lets us render system
// as "rules + character" only and put world state into a separate
// user message with a cache-pointe on Anthropic and a clean
// prefix on OpenAI.
type CharacterContext struct {
	// Display name (e.g. "Маркус"). Used as the section header
	// in the system prompt.
	Character string
	// SOUL.md contents — who the character is.
	CharacterSOUL string
	// SKILL.md contents — what the character can do.
	CharacterSKILL string
	// memory.md contents — cross-multiverse memories.
	CharacterMemory string
	// CharacterSections lists `## <name>` headers currently
	// present in the character's three files. Rendered into
	// the system prompt so the model picks an existing
	// section name (no guessing, no duplicates) when issuing
	// `update_character`. Empty until the character has at
	// least one file body.
	CharacterSections string
}

// WorldContext is the per-turn bundle of active-world data:
// the world's world state, canon, lore, plan, chronicle and
// the profiles of NPCs currently in the scene. Everything in
// here depends on the active world and changes only on
// end_day / leave_world / /reload / compaction, so it goes
// into a separate user message with a cache-pointe on Anthropic.
//
// World data used to live inside BuildSystemPrompt's
// PromptContext alongside character data. Splitting it
// out makes the cache boundary explicit: system changes
// only on character edits or narrative.md changes; user[0]
// changes only on world-state mutations.
type WorldContext struct {
	// Display name of the active world (e.g. "naruto").
	// Used as the section header in the user message.
	World string
	// canon.md — operator-owned external canon, never edited
	// by the bot. Read-only here.
	WorldCanon string
	// state.md — current day, in_flight flag, moment, NPCs,
	// хронология, Хроника текущего дня, Протокол прошедших
	// дней. The "здесь и сейчас" view.
	WorldState string
	// plan.md — 3-5 upcoming events the player committed to.
	WorldPlan string
	// lore.md — canon deviations accumulated in this playthrough.
	WorldLore string
	// Chronicle is the rendered history of the world:
	// one entry per LLM-compressed 30-day window plus
	// the raw per-day log for the open window. Either
	// field may be empty (a fresh world has neither;
	// a long-running world may have many periods and
	// an empty open window). Nil Chronicle means the
	// renderer hides both blocks.
	Chronicle *Chronicle
	// NPCs currently in the scene, rendered as compact
	// profiles. The active roster is the slice of characters
	// the LLM needs to know about right now.
	NPCs []NPCSnapshot
	// WorldStage is the rendered active stage of the world's
	// staged story graph. Empty when the world is a sandbox
	// (staging.yaml enabled=false) or no staging is configured.
	WorldStage string
	// NPCRegistry is the compact list of every NPC known to
	// the world (slug + display_name + nicknames). Embedded
	// at the top of user[0] so the LLM can resolve names to
	// slugs without calling search_npc on every new turn.
	// Empty when the world has no characters.yaml yet
	// (firstlaunch path) or when staging is disabled.
	NPCRegistry string
}

// Chronicle is the rendered history of one world.
// Periods are LLM-compressed 30-day windows; Days
// are the raw per-day log for the currently-open
// (most-recent, unclosed) window. The renderer
// iterates Periods in From-order (append-order —
// the compression hook never re-orders) and Days
// sorted by Number ascending.
type Chronicle struct {
	Periods []ChroniclePeriod
	Days    []ChronicleDay
}

// ChroniclePeriod is one LLM-compressed window covering
// raw days [From..To]. Memory is the distilled essence
// the summarizer produced; the renderer emits it as
// "с <From> по <To> дни: <Memory>".
type ChroniclePeriod struct {
	From   int
	To     int
	Memory string
}

// ChronicleDay is one raw per-day entry. Number is the
// day counter; Text is the player-supplied narrative
// summary that ArchiveChronicleDay recorded. The renderer
// emits it as "День <Number>: <Text>".
type ChronicleDay struct {
	Number int
	Text   string
}

// StateSnapshot is the in-memory representation of
// state.md. The writer-tool layer (usecase/tools/files)
// reads/writes this struct; the data-bag for the
// state.md.tmpl template (prompts.StateData) is
// projected from it in renderStateBody.
type StateSnapshot struct {
	World    string
	Day      int
	InFlight bool
	Location string
	NPCs     []string
	Moment   string
	Events   []string
}

// NPCEntry is a single row of the NPC registry summary
// embedded in the WorldState user message. The LLM uses
// this to map names it sees in `state.md` (e.g. "Какаши
// Хатаке" in the `NPC:` line) to slugs the file system
// expects ("hatake_kakashi.yaml"). The model never has to
// guess — the slug is right there.
type NPCEntry struct {
	Slug        string
	DisplayName string
	Nicknames   []string
}

// NPCSnapshot is a per-NPC mini-card the GM sees for any NPC active
// in the current scene. info-isolation is enforced upstream — NPCs
// that should not know about a topic simply do not appear in the
// snapshot.
type NPCSnapshot struct {
	DisplayName string
	Profile     string // full file contents, sanitised
}

// BuildSystemPrompt renders the system message (Индекс 0) for the
// narrative role. The static skill text is passed in by the caller
// from prompts/narrative.md (or rendered from
// prompts/narrative.md.tmpl) — this function only handles the
// technical prefix (/no_think). The character-block (SOUL/SKILL/
// memory) used to live here, but as of the templates refactor it
// moved into world_state.md.tmpl (user[0]) — see
// BuildWorldStateMessage and the project-wide rule "system and
// world state are two different messages in []completions".
//
// disableThinking prepends a "/no_think" directive to the system
// prompt. The primary switch for Ollama v0.5.11+ thinking-capable
// models (Qwen3, DeepSeek R1) is the top-level `think: false` on
// the wire payload — that is the supported, documented path. The
// in-prompt sentinel is a redundant reminder for models that do
// happen to recognise Qwen-style "/no_think" tokens, and a
// softener for operators who do not realise the wire flag exists.
// It costs ~5 extra tokens per turn; the operator can disable
// the role-level flag if they want zero overhead.
func BuildSystemPrompt(staticRules string, char CharacterContext, disableThinking ...bool) string {
	_ = char // kept for callers that pre-populate CharacterContext;
	// character-block is rendered in user[0] via
	// BuildWorldStateMessage, not here.
	noThink := len(disableThinking) > 0 && disableThinking[0]
	if !noThink {
		return strings.TrimSpace(staticRules)
	}
	return "/no_think\n\n" + strings.TrimSpace(staticRules)
}

// BuildWorldStateMessage renders the WorldState user message
// (Индекс 1) — the cache-pointe that holds the active world's
// data. Anthropic driver attaches `cache_control: ephemeral` to
// this block; OpenAI uses prefix-cache on the same prefix.
//
// The block opens with a `[WORLD_STATE]` marker so the LLM
// recognises it on re-read. After the marker come the world's
// world state (here-and-now), the active character block
// (SOUL/SKILL/memory), the NPC registry, canon (external,
// read-only), lore (deviations), plan (3-5 upcoming events),
// chronicle (full history including compressed windows), the
// staged story (sюжетная стадия) and finally the active NPC
// profiles.
//
// All formatting lives in prompts/world_state.md.tmpl — this
// function is a thin wrapper that builds the data-bag and
// delegates to prompts.Render. Order of blocks, conditional
// rendering, and bullet formatting are template decisions, not
// Go decisions.
//
// The block is regenerated on:
//   - end_day (after the protocol is appended)
//   - leave_world
//   - /reload
//   - compaction (after the chronicle is appended)
//
// Between those events the snapshot is reused verbatim — see
// gm.worldSnapshot. Every other mutation (`update_state`,
// `create_npc`, `update_npc`, ...) is reflected to the LLM
// through a short ToolResult delta instead of invalidating
// this block.
func BuildWorldStateMessage(world WorldContext, char CharacterContext) (string, error) {
	charData := prompts.CharacterData{
		Name:   char.Character,
		SOUL:   char.CharacterSOUL,
		SKILL:  char.CharacterSKILL,
		Memory: char.CharacterMemory,
	}
	worldData := prompts.WorldData{
		Name:        world.World,
		State:       world.WorldState,
		Canon:       world.WorldCanon,
		Lore:        world.WorldLore,
		Plan:        world.WorldPlan,
		Chronicle:   convertChronicle(world.Chronicle),
		Stage:       world.WorldStage,
		NPCRegistry: world.NPCRegistry,
	}
	if world.NPCs != nil {
		displayNames := make([]string, len(world.NPCs))
		profiles := make([]string, len(world.NPCs))
		for i, n := range world.NPCs {
			displayNames[i] = n.DisplayName
			profiles[i] = n.Profile
		}
		worldData.ActiveNPCs = prompts.ConvertNPCSnapshots(displayNames, profiles)
	}
	return prompts.Render("world_state.md.tmpl", prompts.PromptData{
		Character: charData,
		World:     worldData,
	})
}

// FormatNPCRegistry was the helper that rendered the
// world's NPC registry as a compact markdown list. The
// formatting now lives inline in
// prompts/world_state.md.tmpl. The function has been
// removed as part of the templates refactor; this
// stub is kept as a compile-time marker that the
// template owns the format. If you are migrating code
// that still calls FormatNPCRegistry, switch to
// populating WorldContext.NPCRegistry directly with
// a pre-rendered string assembled in gm.go (or wire
// the new structured NPCEntry rendering through the
// template).

// Note: the formatting rules from the old
// FormatNPCRegistry implementation have been moved
// inline into prompts/world_state.md.tmpl. Operators
// that need to debug a missing-slug issue should look
// at the world_state template, not the domain
// package.

// convertChronicle projects the domain-level Chronicle
// (Periods + Days) into the prompts-package shape used
// by world_state.md.tmpl. nil → nil (the template sees
// the {{ if .World.Chronicle }} guard and hides both
// blocks). A non-nil Chronicle with empty Periods and
// empty Days renders as "no periods, no days" — which
// is the same as nil for our purposes, but we keep it
// distinct so callers can express "we have looked at
// the file and it has nothing" vs "we never read the
// file".
func convertChronicle(in *Chronicle) *prompts.ChronicleData {
	if in == nil {
		return nil
	}
	out := &prompts.ChronicleData{
		Periods: make([]prompts.ChroniclePeriodData, len(in.Periods)),
		Days:    make([]prompts.ChronicleDayData, len(in.Days)),
	}
	for i, p := range in.Periods {
		out.Periods[i] = prompts.ChroniclePeriodData{
			From:   p.From,
			To:     p.To,
			Memory: p.Memory,
		}
	}
	for i, d := range in.Days {
		out.Days[i] = prompts.ChronicleDayData{
			Number: d.Number,
			Text:   d.Text,
		}
	}
	return out
}
