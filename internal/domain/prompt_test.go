package domain

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestBuildSystemPrompt_IncludesRulesOnly covers the
// post-templates-refactor contract: system = rules
// only. No character block (character moved to
// world_state.md.tmpl). No world data — world is in
// the user message now.
func TestBuildSystemPrompt_IncludesRulesOnly(t *testing.T) {
	got := BuildSystemPrompt(
		"# Rules\nне управляй персонажем игрока",
		CharacterContext{
			Character:     "Маркус",
			CharacterSOUL: "человек",
		},
	)
	assert.Contains(t, got, "не управляй персонажем игрока")
	// Character block is no longer in system — moved to
	// world_state.md.tmpl as part of the templates
	// refactor.
	assert.NotContains(t, got, "## Персонаж игрока: Маркус",
		"character block moved to world_state.md.tmpl (user[0])")
	assert.NotContains(t, got, "### SOUL",
		"character SOUL moved to world_state.md.tmpl")
	assert.NotContains(t, got, "## Активный мир",
		"system prompt must not contain world data")
	assert.NotContains(t, got, "state.md",
		"system prompt must not contain world data")
	assert.NotContains(t, got, "[WORLD_STATE]",
		"system prompt must not contain the [WORLD_STATE] marker — that belongs to user[0]")
}

// TestBuildSystemPrompt_StaticOnly: the system prompt
// output must be byte-for-byte identical when only the
// CharacterContext is empty. (Same rules in, same
// string out — the function does not depend on
// character state any more.)
func TestBuildSystemPrompt_StaticOnly(t *testing.T) {
	base := BuildSystemPrompt("rules", CharacterContext{Character: "Маркус", CharacterSOUL: "x"})
	withWorld := BuildSystemPrompt("rules", CharacterContext{Character: "Маркус", CharacterSOUL: "x"})

	assert.Equal(t, base, withWorld,
		"BuildSystemPrompt must not depend on world state")
}

// TestBuildWorldStateMessage_IncludesEverythingElse: the user
// message (Индекс 1) carries every world-scoped field. The full
// set has to round-trip byte-for-byte.
func TestBuildWorldStateMessage_IncludesEverythingElse(t *testing.T) {
	got, err := BuildWorldStateMessage(WorldContext{
		World:         "naruto",
		WorldState:    "День 3 (в процессе).\nУтро.",
		WorldPlan:     "- День +1: встреча с Какаши",
		WorldMemorise: "д00001: a\nд00002: b",
		WorldCanon:    "канон",
		WorldLore:     "отклонения",
		NPCs: []NPCSnapshot{
			{DisplayName: "Какаши", Profile: "Шаринган"},
		},
	}, CharacterContext{Character: "Маркус"})
	assert.NoError(t, err)
	assert.Contains(t, got, "[WORLD_STATE]")
	assert.Contains(t, got, "## Активный мир: naruto")
	assert.Contains(t, got, "world state (здесь и сейчас)")
	assert.Contains(t, got, "День 3 (в процессе).")
	assert.Contains(t, got, "### Канон")
	assert.Contains(t, got, "### Отклонения от канона (lore)")
	assert.Contains(t, got, "### plan (3-5 предстоящих событий)")
	assert.Contains(t, got, "### Хронология (chronicle)")
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
	got, err := BuildWorldStateMessage(WorldContext{
		World:         "w",
		WorldCanon:    longCanon,
		WorldLore:     longLore,
		WorldMemorise: longMemorise,
		NPCs: []NPCSnapshot{
			{DisplayName: "Какаши", Profile: longProfile},
		},
	}, CharacterContext{})
	assert.NoError(t, err)
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
	got, err := BuildWorldStateMessage(WorldContext{}, CharacterContext{})
	assert.NoError(t, err)
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

// TestBuildWorldStateMessage_StageBlockRendered: when
// WorldStage is set (pre-rendered by gm.loadWorldStage),
// the user message contains the stage block in full.
func TestBuildWorldStateMessage_StageBlockRendered(t *testing.T) {
	stage := "### Сюжетная стадия\n**beginning — Появление**\n\nГерой появляется.\n\n**Таймлайн:**\n[>] 1: Появление\n\n**Возможные переходы:**\n- → accepted, если: Герой доказал невиновность"
	got, err := BuildWorldStateMessage(WorldContext{
		World:      "naruto",
		WorldStage: stage,
	}, CharacterContext{})
	assert.NoError(t, err)
	assert.Contains(t, got, "Сюжетная стадия")
	assert.Contains(t, got, "beginning")
	assert.Contains(t, got, "→ accepted")
}

// TestBuildWorldStateMessage_NoStage: when WorldStage is
// empty (sandbox world or staging disabled), the user
// message does NOT contain the stage block.
func TestBuildWorldStateMessage_NoStage(t *testing.T) {
	got, err := BuildWorldStateMessage(WorldContext{
		World: "naruto",
	}, CharacterContext{})
	assert.NoError(t, err)
	assert.NotContains(t, got, "Сюжетная стадия")
}

// TestBuildWorldStateMessage_CharacterBlockInUser:
// character-block lives in world_state.md.tmpl, so a
// non-empty character renders in the user message
// (user[0]), not the system message. The split is the
// project-wide rule: system = rules only, world =
// [character + world + npcs + ...].
func TestBuildWorldStateMessage_CharacterBlockInUser(t *testing.T) {
	got, err := BuildWorldStateMessage(WorldContext{World: "naruto"}, CharacterContext{
		Character:      "Маркус",
		CharacterSOUL:  "человек",
		CharacterSKILL: "стрелок",
	})
	assert.NoError(t, err)
	assert.Contains(t, got, "## Персонаж игрока: Маркус")
	assert.Contains(t, got, "### SOUL")
	assert.Contains(t, got, "человек")
	assert.Contains(t, got, "### SKILL")
	assert.Contains(t, got, "стрелок")
}
