package domain

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBuildSystemPrompt_IncludesSkillAndContext(t *testing.T) {
	got := BuildSystemPrompt(
		"# Rules\nне управляй персонажем игрока",
		PromptContext{
			Character:      "Маркус",
			CharacterSOUL:  "человек",
			World:          "naruto",
			WorldState:     "День 3 (в процессе).\nУтро.",
			WorldPlan:      "- День +1: встреча с Какаши",
			WorldMemorise: "д00001: a\nд00002: b",
		},
	)
	assert.Contains(t, got, "не управляй персонажем игрока")
	assert.Contains(t, got, "## Персонаж игрока: Маркус")
	assert.Contains(t, got, "## Активный мир: naruto")
	assert.Contains(t, got, "state.md (здесь и сейчас)")
	assert.Contains(t, got, "День 3 (в процессе).")
}

func TestBuildSystemPrompt_PassesLongCanonAndLoreThroughUncut(t *testing.T) {
	longCanon := strings.Repeat("A", 5000)
	longLore := strings.Repeat("B", 5000)
	got := BuildSystemPrompt("rules", PromptContext{
		World:      "w",
		WorldCanon: longCanon,
		WorldLore:  longLore,
	})
	assert.NotContains(t, got, "[…truncated…]",
		"system prompt must not self-edit canon/lore — summarisation lives in maintain_lore")
	assert.Contains(t, got, longCanon)
	assert.Contains(t, got, longLore)
}

func TestBuildSystemPrompt_PassesMemoriseThroughUncut(t *testing.T) {
	longMemorise := strings.Repeat("M", 10000)
	got := BuildSystemPrompt("rules", PromptContext{
		World:         "w",
		WorldMemorise: longMemorise,
	})
	assert.NotContains(t, got, "[…truncated…]",
		"system prompt must not self-edit memorise — summarisation lives in the ArchiveDay hook")
	assert.Contains(t, got, longMemorise)
}

func TestBuildSystemPrompt_PassesLongNPCProfileThroughUncut(t *testing.T) {
	longProfile := strings.Repeat("X", 3000)
	got := BuildSystemPrompt("rules", PromptContext{
		World: "w",
		NPCs: []NPCSnapshot{
			{DisplayName: "Какаши", Profile: longProfile},
		},
	})
	assert.NotContains(t, got, "[…truncated…]",
		"system prompt must not self-edit NPC profiles — summarisation lives in maintain_npcs")
	assert.Contains(t, got, longProfile)
}

func TestBuildSystemPrompt_NoCharacterNoWorld(t *testing.T) {
	got := BuildSystemPrompt("rules", PromptContext{})
	assert.Contains(t, got, "rules")
	assert.NotContains(t, got, "## Персонаж")
	assert.NotContains(t, got, "## Активный мир")
}

func TestBuildSystemPrompt_RendersNPCs(t *testing.T) {
	got := BuildSystemPrompt("rules", PromptContext{
		World: "w",
		NPCs: []NPCSnapshot{
			{DisplayName: "Какаши", Profile: "Шаринган"},
			{DisplayName: "Саске", Profile: "Учиха"},
		},
	})
	assert.Contains(t, got, "## Активные NPC")
	assert.Contains(t, got, "### Какаши")
	assert.Contains(t, got, "Шаринган")
	assert.Contains(t, got, "### Саске")
}
