package usecase_test

import (
	"testing"

	"github.com/bestxp/narrative-ai-agent/internal/structured"
	"github.com/bestxp/narrative-ai-agent/internal/usecase"
	"github.com/stretchr/testify/assert"
)

func TestResponseFormat_AllBlocksPresent(t *testing.T) {
	t.Parallel()

	rf := usecase.NewResponseFormat(350, "ru")
	body := structured.HeaderDialogue + `
Привет.

` + structured.HeaderContext + `
files: state.md

` + structured.HeaderFuture + `
- Ближайшие события: продолжение сцены

` + structured.HeaderValidation + `
- Лимит слов: 2 / 350
- Управлял персонажем игрока: нет
- NPC знал только то, что должен: да
- Файлы обновлены: state.md
`
	v := rf.Validate(body)
	assert.True(t, v.HasDialogue, "missing dialogue header")
	assert.True(t, v.HasContextBlock, "missing context header")
	assert.True(t, v.HasFutureBlock, "missing future header")
	assert.True(t, v.HasValidationBlk, "missing validation header")
}

func TestResponseFormat_OverLimit(t *testing.T) {
	t.Parallel()

	rf := usecase.NewResponseFormat(5, "ru")
	body := "один два три четыре пять шесть семь"
	v := rf.Validate(body)
	assert.True(t, v.OverLimit)
}

func TestResponseFormat_ForbiddenForms(t *testing.T) {
	t.Parallel()

	rf := usecase.NewResponseFormat(350, "ru")
	body := "ты усмехнулся, потом подумал."
	v := rf.Validate(body)
	assert.NotEmpty(t, v.ForbiddenForms)
}

func TestResponseFormat_WordCount_Russian(t *testing.T) {
	t.Parallel()

	rf := usecase.NewResponseFormat(350, "ru")
	body := "раз два три"
	assert.Equal(t, 3, rf.Validate(body).WordCount)
}

func TestResponseFormat_RejectsCJK(t *testing.T) {
	t.Parallel()

	rf := usecase.NewResponseFormat(350, "ru")
	body := "Привет \u65e5\u672c\u8a9e"
	v := rf.Validate(body)
	assert.False(t, v.LatinOnly)
}
