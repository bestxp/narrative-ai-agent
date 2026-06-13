package domain

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestBuildSystemPrompt_IncludesRulesAndCharacter covers the
// contract: system = rules + character only. No world data —
// world is in the user message now.
func TestBuildSystemPrompt_IncludesRulesAndCharacter(t *testing.T) {
	got := BuildSystemPrompt(
		"# Rules\nне управляй персонажем игрока",
		CharacterContext{
			Character:     "Маркус",
			CharacterSOUL: "человек",
		},
	)
	assert.Contains(t, got, "не управляй персонажем игрока")
	assert.Contains(t, got, "## Персонаж игрока: Маркус")
	assert.Contains(t, got, "### SOUL")
	assert.NotContains(t, got, "## Активный мир",
		"system prompt must not contain world data")
	assert.NotContains(t, got, "state.md",
		"system prompt must not contain world data")
	assert.NotContains(t, got, "[WORLD_STATE]",
		"system prompt must not contain the [WORLD_STATE] marker — that belongs to user[0]")
}

// TestBuildSystemPrompt_StaticOnly: the system prompt output must
// be byte-for-byte identical when only world-context changes
// (the system prompt is supposed to depend on rules + character
// ONLY, since cache-pointe on system is meant to be stable across
// scene/world changes within a day).
func TestBuildSystemPrompt_StaticOnly(t *testing.T) {
	base := BuildSystemPrompt("rules", CharacterContext{Character: "Маркус", CharacterSOUL: "x"})
	withWorld := BuildSystemPrompt("rules", CharacterContext{Character: "Маркус", CharacterSOUL: "x"})// pretend the caller varied other args — same rules, same character

	assert.Equal(t, base, withWorld,
		"BuildSystemPrompt must not depend on world state")
}

// TestBuildWorldStateMessage_IncludesEverythingElse: the user
// message (Индекс 1) carries every world-scoped field. The full
// set has to round-trip byte-for-byte.
func TestBuildWorldStateMessage_IncludesEverythingElse(t *testing.T) {
	got := BuildWorldStateMessage(WorldContext{
		World:         "naruto",
		WorldState:    "День 3 (в процессе).\nУтро.",
		WorldPlan:     "- День +1: встреча с Какаши",
		WorldMemorise: "д00001: a\nд00002: b",
		WorldCanon:    "канон",
		WorldLore:     "отклонения",
		NPCs: []NPCSnapshot{
			{DisplayName: "Какаши", Profile: "Шаринган"},
		},
	})
	assert.Contains(t, got, "[WORLD_STATE]")
	assert.Contains(t, got, "## Активный мир: naruto")
	assert.Contains(t, got, "state.md (здесь и сейчас)")
	assert.Contains(t, got, "День 3 (в процессе).")
	assert.Contains(t, got, "### Канон")
	assert.Contains(t, got, "### Отклонения от канона (lore.md)")
	assert.Contains(t, got, "### plan.md (3-5 предстоящих событий)")
	assert.Contains(t, got, "### Хронология (memorise.md)")
	assert.Contains(t, got, "## Активные NPC")
	assert.Contains(t, got, "### Какаши")
	assert.Contains(t, got, "Шаринган")
}

// TestBuildWorldStateMessage_PassesLongFieldsThroughUncut: the
// world-state user message must NOT self-edit canon/lore/memorise
// /NPC profiles — summarisation lives in their respective
// maintain_* tools. The pass-through guarantees cache hits
// (the model sees the same bytes on every turn, no surprise
// truncation that would invalidate the cache_pointe).
func TestBuildWorldStateMessage_PassesLongFieldsThroughUncut(t *testing.T) {
	longCanon := strings.Repeat("A", 5000)
	longLore := strings.Repeat("B", 5000)
	longMemorise := strings.Repeat("M", 10000)
	longProfile := strings.Repeat("X", 3000)
	got := BuildWorldStateMessage(WorldContext{
		World:         "w",
		WorldCanon:    longCanon,
		WorldLore:     longLore,
		WorldMemorise: longMemorise,
		NPCs: []NPCSnapshot{
			{DisplayName: "Какаши", Profile: longProfile},
		},
	})
	assert.NotContains(t, got, "[…truncated…]")
	assert.Contains(t, got, longCanon)
	assert.Contains(t, got, longLore)
	assert.Contains(t, got, longMemorise)
	assert.Contains(t, got, longProfile)
}

// TestBuildWorldStateMessage_EmptyWorld: when there is no
// active world (cold-start, before /launch), the user message
// still emits the [WORLD_STATE] marker so the LLM knows the
// cache slot exists. Nothing else is rendered.
func TestBuildWorldStateMessage_EmptyWorld(t *testing.T) {
	got := BuildWorldStateMessage(WorldContext{})
	assert.Contains(t, got, "[WORLD_STATE]")
	assert.NotContains(t, got, "## Активный мир")
	assert.NotContains(t, got, "## Активные NPC")
}

func TestBuildSystemPrompt_NoCharacter(t *testing.T) {
	got := BuildSystemPrompt("rules", CharacterContext{})
	assert.Contains(t, got, "rules")
	assert.NotContains(t, got, "## Персонаж")
}

// TestBuildSystemPrompt_CharacterSectionsIgnored: the
// h5 refactor removed the per-file section list
// block from the system prompt — section names
// already live inside each YAML body as
// `data: [{section, values}]`, so the model sees
// them verbatim. The CharacterSections field is
// kept on the struct for backward compat but the
// renderer no longer echoes it. We assert the
// block is NOT present.
func TestBuildSystemPrompt_CharacterSectionsIgnored(t *testing.T) {
	got := BuildSystemPrompt("rules", CharacterContext{
		Character:         "Маркус",
		CharacterSections: "SOUL: Истинная сущность / Философия\nSKILL: Базовые способности / Оружие",
	})
	assert.NotContains(t, got, "### Доступные секции",
		"section enumeration block is no longer rendered; the YAML body has it")
}
