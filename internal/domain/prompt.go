package domain

import (
	"fmt"
	"strings"
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
// the world's state.md, canon, lore, plan, memorise and the
// profiles of NPCs currently in the scene. Everything in
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
	// memorise.md — full file, including earlier compressed
	// 30-day windows. The "история" view.
	WorldMemorise string
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
// narrative role. It embeds the static skill text (passed in by
// the caller from prompts/narrative.md) and the player-character
// data (SOUL/SKILL/memory + the section list for update_character).
// NOTHING world-related lives here — the world state goes into a
// separate user message (see BuildWorldStateMessage).
//
// Format is intentionally human-readable: the LLM re-reads this
// every turn, and a markdown layout is easier to navigate than a
// JSON blob.
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
	noThink := len(disableThinking) > 0 && disableThinking[0]
	var b strings.Builder
	if noThink {
		b.WriteString("/no_think\n")
	}
	b.WriteString(strings.TrimSpace(staticRules))
	b.WriteString("\n\n---\n\n")

	if char.Character != "" {
		fmt.Fprintf(&b, "## Персонаж игрока: %s\n\n", char.Character)
		if char.CharacterSOUL != "" {
			b.WriteString("### SOUL\n")
			b.WriteString(char.CharacterSOUL)
			b.WriteString("\n\n")
		}
		if char.CharacterSKILL != "" {
			b.WriteString("### SKILL\n")
			b.WriteString(char.CharacterSKILL)
			b.WriteString("\n\n")
		}
		if char.CharacterMemory != "" {
			b.WriteString("### Межмировые воспоминания\n")
			b.WriteString(char.CharacterMemory)
			b.WriteString("\n\n")
		}
		// CharacterSections is intentionally left
		// UNUSED. The h5 refactor moved the per-file
		// data into the YAML bodies themselves
		// (each file's `data: [{section, values}]`
		// array is the canonical section list) —
		// the model sees section names verbatim in
		// the SOUL/SKILL/memory bodies above. A
		// separate enumeration block would just
		// duplicate the YAML keys and add cache
		// weight to the system prompt. The field
		// is kept on CharacterContext for backward
		// compatibility with external callers that
		// pre-populate it; we just do not render it.
	}

	return strings.TrimSpace(b.String())
}

// BuildWorldStateMessage renders the WorldState user message
// (Индекс 1) — the cache-pointe that holds the active world's
// data. Anthropic driver attaches `cache_control: ephemeral` to
// this block; OpenAI uses prefix-cache on the same prefix.
//
// The block opens with a `[WORLD_STATE]` marker so the LLM
// recognises it on re-read. After the marker come the world's
// world state (here-and-now), canon (external, read-only), lore
// (deviations), plan (3-5 upcoming events), chronicle (full
// history including compressed windows), and finally the
// active NPC profiles.
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
func BuildWorldStateMessage(world WorldContext) string {
	var b strings.Builder
	b.WriteString("[WORLD_STATE]\n")

	if world.World != "" {
		fmt.Fprintf(&b, "\n## Активный мир: %s\n\n", world.World)
		if world.WorldState != "" {
			b.WriteString("### world state (здесь и сейчас)\n")
			b.WriteString(world.WorldState)
			b.WriteString("\n\n")
		}
		// Реестр NPC — встроен в самом верху, чтобы LLM
		// видел «display_name → slug» без необходимости
		// дёргать search_npc на каждом ходу. Помогает при
		// create_npc/update_npc — LLM берёт slug прямо
		// из реестра, а не выдумывает транслитерацию.
		if world.NPCRegistry != "" {
			b.WriteString(world.NPCRegistry)
			b.WriteString("\n\n")
		}
		if world.WorldCanon != "" {
			b.WriteString("### Канон\n")
			b.WriteString(world.WorldCanon)
			b.WriteString("\n\n")
		}
		if world.WorldLore != "" {
			b.WriteString("### Отклонения от канона (lore)\n")
			b.WriteString(world.WorldLore)
			b.WriteString("\n\n")
		}
		if world.WorldPlan != "" {
			b.WriteString("### plan (3-5 предстоящих событий)\n")
			b.WriteString(world.WorldPlan)
			b.WriteString("\n\n")
		}
		if world.WorldMemorise != "" {
			b.WriteString("### Хронология (chronicle)\n")
			b.WriteString(world.WorldMemorise)
			b.WriteString("\n\n")
		}
		if world.WorldStage != "" {
			b.WriteString(world.WorldStage)
			b.WriteString("\n\n")
		}
	}

	if len(world.NPCs) > 0 {
		b.WriteString("## Активные NPC\n")
		for _, npc := range world.NPCs {
			fmt.Fprintf(&b, "### %s\n", npc.DisplayName)
			b.WriteString(npc.Profile)
			b.WriteString("\n\n")
		}
	}

	return strings.TrimSpace(b.String())
}

// FormatNPCRegistry renders the world's NPC registry as
// a compact map the LLM can read in one glance. The slug
// is the first thing printed so the model can copy it
// verbatim into a tool call (create_npc, update_npc,
// search_npc) without re-deriving it from a Russian
// display_name. Nicknames follow in parentheses so the
// model can also recognise short forms the operator may
// have used in state.md ("Саске" → "sasuke_uchiha").
//
// When the registry is empty (first launch, sandbox
// world, fresh characters.yaml that has not been
// populated yet) the helper returns "" so the
// WorldState block does not gain an empty header.
//
// Format (markdown list, one line per NPC):
//
//	## Ниже информация по известным NPC <world>
//	* <display_name> (<nick1>, <nick2>, ...): <slug>
//	* <display_name> (...): <slug>
func FormatNPCRegistry(worldName string, entries []NPCEntry) string {
	if len(entries) == 0 {
		return ""
	}
	var b strings.Builder
	if worldName == "" {
		b.WriteString("## Ниже информация по известным NPC\n")
	} else {
		fmt.Fprintf(&b, "## Ниже информация по известным NPC (%s)\n", worldName)
	}
	for _, e := range entries {
		// Drop the empty/blank nicknames to avoid
		// "<display_name> (): <slug>".
		parts := make([]string, 0, len(e.Nicknames))
		for _, n := range e.Nicknames {
			if t := strings.TrimSpace(n); t != "" {
				parts = append(parts, t)
			}
		}
		display := strings.TrimSpace(e.DisplayName)
		slug := strings.TrimSpace(e.Slug)
		if display == "" {
			display = slug
		}
		if len(parts) > 0 {
			fmt.Fprintf(&b, "* %s (%s): %s\n", display, strings.Join(parts, ", "), slug)
		} else {
			fmt.Fprintf(&b, "* %s: %s\n", display, slug)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
