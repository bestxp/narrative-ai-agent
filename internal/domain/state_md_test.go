package domain

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBuildStateMarkdown_BasicMoment(t *testing.T) {
	out := BuildStateMarkdown(StateSnapshot{
		World:    "naruto",
		Day:      5,
		InFlight: true,
		Location: "Коноха, допрос",
		NPCs:     []string{"anbu_dog", "anbu_cat"},
		Moment:   "Аньбу толкает Маркуса в спину.",
	})
	assert.Contains(t, out, "# Состояние мира: naruto")
	assert.Contains(t, out, "## Текущий момент")
	assert.Contains(t, out, "День 5 (в процессе)")
	assert.Contains(t, out, "Локация: Коноха, допрос")
	assert.Contains(t, out, "NPC: anbu_dog, anbu_cat")
	assert.Contains(t, out, "Момент: Аньбу толкает Маркуса в спину")
	assert.NotContains(t, out, "## Хронология дня", "no events → no chronology section")
}

func TestBuildStateMarkdown_WithEvents(t *testing.T) {
	out := BuildStateMarkdown(StateSnapshot{
		World:    "naruto",
		Day:      5,
		InFlight: true,
		Events:   []string{"Ход 12: Аньбу остановила Маркуса", "Ход 13: Маркус назвался"},
	})
	assert.Contains(t, out, "## Хронология дня")
	assert.Contains(t, out, "- Ход 12: Аньбу остановила Маркуса")
	assert.Contains(t, out, "- Ход 13: Маркус назвался")
}

func TestBuildStateMarkdown_DayFinished(t *testing.T) {
	out := BuildStateMarkdown(StateSnapshot{World: "naruto", Day: 5, InFlight: false})
	assert.Contains(t, out, "День 5 (завершён)")
}

func TestBuildStateMarkdown_Empty(t *testing.T) {
	out := BuildStateMarkdown(StateSnapshot{World: "naruto"})
	assert.Contains(t, out, "# Состояние мира: naruto")
	// No "## Текущий момент" if no fields are filled — we still
	// emit the section header so the LLM can rely on the layout.
	assert.True(t, strings.Contains(out, "## Текущий момент"))
}
