package usecase

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestResponseFormat_AllBlocksPresent(t *testing.T) {
	t.Parallel()
	rf := NewResponseFormat(350, "ru")
	body := `**диалоги и действия**
Привет.

**КОНТЕКСТ И ИЗМЕНЕНИЯ**
files: state.md

**БУДУЩЕЕ**
- Ближайшие события: продолжение сцены

**ВАЛИДАЦИЯ ПРАВИЛ**
- Лимит слов: 2 / 350
- Управлял персонажем игрока: нет
- NPC знал только то, что должен: да
- Файлы обновлены: state.md
`
	v := rf.Validate(body)
	assert.True(t, v.HasDialogue, "missing **диалоги и действия**")
	assert.True(t, v.HasContextBlock, "missing **КОНТЕКСТ И ИЗМЕНЕНИЯ**")
	assert.True(t, v.HasFutureBlock, "missing **БУДУЩЕЕ**")
	assert.True(t, v.HasValidationBlk, "missing **ВАЛИДАЦИЯ ПРАВИЛ**")
}

func TestResponseFormat_OverLimit(t *testing.T) {
	t.Parallel()
	rf := NewResponseFormat(5, "ru")
	body := "один два три четыре пять шесть семь"
	v := rf.Validate(body)
	assert.True(t, v.OverLimit)
}

func TestResponseFormat_ForbiddenForms(t *testing.T) {
	t.Parallel()
	rf := NewResponseFormat(350, "ru")
	body := "ты усмехнулся, потом подумал."
	v := rf.Validate(body)
	assert.NotEmpty(t, v.ForbiddenForms)
}

func TestResponseFormat_WordCount_Russian(t *testing.T) {
	t.Parallel()
	rf := NewResponseFormat(350, "ru")
	body := "раз два три"
	assert.Equal(t, 3, rf.Validate(body).WordCount)
}

func TestResponseFormat_RejectsCJK(t *testing.T) {
	t.Parallel()
	rf := NewResponseFormat(350, "ru")
	body := "Привет 日本語"
	v := rf.Validate(body)
	assert.False(t, v.LatinOnly)
}
