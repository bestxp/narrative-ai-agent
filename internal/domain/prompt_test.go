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
			WorldMemoriseTail: "д00001: a\nд00002: b",
		},
	)
	assert.Contains(t, got, "не управляй персонажем игрока")
	assert.Contains(t, got, "## Персонаж игрока: Маркус")
	assert.Contains(t, got, "## Активный мир: naruto")
	assert.Contains(t, got, "state.md (здесь и сейчас)")
	assert.Contains(t, got, "День 3 (в процессе).")
}

func TestBuildSystemPrompt_TruncatesLongCanon(t *testing.T) {
	long := strings.Repeat("A", 5000)
	got := BuildSystemPrompt("rules", PromptContext{World: "w", WorldCanon: long})
	assert.Contains(t, got, "[…truncated…]")
	assert.NotContains(t, got, strings.Repeat("A", 5000))
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

func TestTruncate(t *testing.T) {
	assert.Equal(t, "", truncate("anything", 0))
	assert.Equal(t, "abc", truncate("abc", 10))
	out := truncate("abcdef", 3)
	assert.Equal(t, "abc\n[…truncated…]", out)
}
