package domain

import (
	"fmt"
	"strings"
)

// PromptContext is the per-turn "what the GM should know" bundle.
// It is constructed by the GM usecase at the start of every reply
// and contains everything from game-data that the LLM is allowed to
// see — characters the player trusts with, the active world's state,
// lore deviations, and the player character's memory of past
// multiverse crossings.
type PromptContext struct {
	Character         string
	CharacterSOUL     string
	CharacterSKILL    string
	CharacterMemory   string
	World             string
	WorldCanon        string
	WorldState        string
	WorldPlan         string
	WorldLore         string
	WorldMemoriseTail string // last ~20 days, no archive
	NPCs              []NPCSnapshot
}

// NPCSnapshot is a per-NPC mini-card the GM sees for any NPC active
// in the current scene. info-isolation is enforced upstream — NPCs
// that should not know about a topic simply do not appear in the
// snapshot.
type NPCSnapshot struct {
	DisplayName string
	Profile     string // full file contents, sanitised
}

// BuildSystemPrompt renders the system message for the narrative
// role. It embeds the static skill text (passed in by the caller
// from prompts/narrative.md) and follows it with the dynamic
// per-turn context.
//
// The format is intentionally human-readable: the LLM is going to
// re-read this every turn, and a markdown layout is easier to
// navigate than a JSON blob.
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
func BuildSystemPrompt(staticRules string, ctx PromptContext, disableThinking ...bool) string {
	noThink := len(disableThinking) > 0 && disableThinking[0]
	var b strings.Builder
	if noThink {
		b.WriteString("/no_think\n")
	}
	b.WriteString(strings.TrimSpace(staticRules))
	b.WriteString("\n\n---\n\n# Текущая сессия\n\n")

	if ctx.Character != "" {
		fmt.Fprintf(&b, "## Персонаж игрока: %s\n\n", ctx.Character)
		if ctx.CharacterSOUL != "" {
			b.WriteString("### SOUL\n")
			b.WriteString(ctx.CharacterSOUL)
			b.WriteString("\n\n")
		}
		if ctx.CharacterSKILL != "" {
			b.WriteString("### SKILL\n")
			b.WriteString(ctx.CharacterSKILL)
			b.WriteString("\n\n")
		}
		if ctx.CharacterMemory != "" {
			b.WriteString("### Межмировые воспоминания\n")
			b.WriteString(ctx.CharacterMemory)
			b.WriteString("\n\n")
		}
	}

	if ctx.World != "" {
		fmt.Fprintf(&b, "## Активный мир: %s\n\n", ctx.World)
		if ctx.WorldCanon != "" {
			b.WriteString("### Канон\n")
			b.WriteString(truncate(ctx.WorldCanon, 4000))
			b.WriteString("\n\n")
		}
		if ctx.WorldState != "" {
			b.WriteString("### state.md (здесь и сейчас)\n")
			b.WriteString(ctx.WorldState)
			b.WriteString("\n\n")
		}
		if ctx.WorldLore != "" {
			b.WriteString("### Отклонения от канона (lore.md)\n")
			b.WriteString(ctx.WorldLore)
			b.WriteString("\n\n")
		}
		if ctx.WorldPlan != "" {
			b.WriteString("### plan.md (3-5 предстоящих событий)\n")
			b.WriteString(ctx.WorldPlan)
			b.WriteString("\n\n")
		}
		if ctx.WorldMemoriseTail != "" {
			b.WriteString("### Хвост memorise.md (последние дни)\n")
			b.WriteString(ctx.WorldMemoriseTail)
			b.WriteString("\n\n")
		}
	}

	if len(ctx.NPCs) > 0 {
		b.WriteString("## Активные NPC\n")
		for _, npc := range ctx.NPCs {
			fmt.Fprintf(&b, "### %s\n", npc.DisplayName)
			b.WriteString(truncate(npc.Profile, 2000))
			b.WriteString("\n\n")
		}
	}

	return strings.TrimSpace(b.String())
}

// truncate caps a body at n runes and adds a marker if the content
// was cut. The cap protects the LLM's context window from very long
// canonical descriptions.
func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "\n[…truncated…]"
}
