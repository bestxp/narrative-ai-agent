package structured

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParsePlain_FullResponse(t *testing.T) {
	t.Parallel()

	input := `------ NARRATIVE ------
День 22, утро. Маркус проснулся в доме Тадзуны.
------ CONTEXT ------
Команда 7 готовится к возвращению в Коноху.
------ FUTURE ------
Завтра — прощание с Тадзуной и отплытие.
------ SYSTEM ------
Лимит слов: 45/150. NPC изолированы.
------ END ------`

	n, err := ParsePlain(input)
	require.NoError(t, err)
	assert.Equal(t, "День 22, утро. Маркус проснулся в доме Тадзуны.", n.Narration)
	assert.Equal(t, "Команда 7 готовится к возвращению в Коноху.", n.Context)
	assert.Equal(t, "Завтра — прощание с Тадзуной и отплытие.", n.Future)
	assert.Equal(t, "Лимит слов: 45/150. NPC изолированы.", n.Validation)
}

func TestParsePlain_PartialStream(t *testing.T) {
	t.Parallel()

	// Only first two sections arrived — the rest is still streaming.
	input := `------ NARRATIVE ------
Маркус вошёл в таверну.
------ CONTEXT ------
В таверне шумно, несколько незнакомых лиц.`

	n, err := ParsePlain(input)
	require.NoError(t, err)
	assert.Equal(t, "Маркус вошёл в таверну.", n.Narration)
	assert.Equal(t, "В таверне шумно, несколько незнакомых лиц.", n.Context)
	assert.Empty(t, n.Future)
	assert.Empty(t, n.Validation)
}

func TestParsePlain_EmptySections(t *testing.T) {
	t.Parallel()

	input := `------ NARRATIVE ------
------ CONTEXT ------
без изменений
------ FUTURE ------
------ SYSTEM ------
------ END ------`

	n, err := ParsePlain(input)
	require.NoError(t, err)
	assert.Empty(t, n.Narration)
	assert.Equal(t, "без изменений", n.Context)
	assert.Empty(t, n.Future)
	assert.Empty(t, n.Validation)
}

func TestParsePlain_StopsAtEND(t *testing.T) {
	t.Parallel()

	// Text after END must be ignored.
	input := `------ NARRATIVE ------
Сцена завершена.
------ END ------
Этот текст должен быть проигнорирован.
И этот тоже.`

	n, err := ParsePlain(input)
	require.NoError(t, err)
	assert.Equal(t, "Сцена завершена.", n.Narration)
	assert.Empty(t, n.Context)
	assert.Empty(t, n.Future)
	assert.Empty(t, n.Validation)
}

func TestParsePlain_CaseInsensitiveMarkers(t *testing.T) {
	t.Parallel()

	input := `------ narrative ------
Текст нарратива.
------ Context ------
Контекст.
------ future ------
Будущее.
------ system ------
Валидация.
------ end ------`

	n, err := ParsePlain(input)
	require.NoError(t, err)
	assert.Equal(t, "Текст нарратива.", n.Narration)
	assert.Equal(t, "Контекст.", n.Context)
	assert.Equal(t, "Будущее.", n.Future)
	assert.Equal(t, "Валидация.", n.Validation)
}

func TestParsePlain_NoMarkers(t *testing.T) {
	t.Parallel()

	n, err := ParsePlain("Просто текст без маркеров.")
	require.NoError(t, err)
	assert.Empty(t, n.Narration)
	assert.Empty(t, n.Context)
	assert.Empty(t, n.Future)
	assert.Empty(t, n.Validation)
}

func TestParsePlain_WhitespaceAroundMarkers(t *testing.T) {
	t.Parallel()

	input := `  ------ NARRATIVE ------

Текст с пустыми строками вокруг.

  ------ CONTEXT ------
Контекст.
  ------ END ------`

	n, err := ParsePlain(input)
	require.NoError(t, err)
	assert.Equal(t, "Текст с пустыми строками вокруг.", n.Narration)
	assert.Equal(t, "Контекст.", n.Context)
}

func TestParsePlain_Render(t *testing.T) {
	t.Parallel()

	n := &Narrative{
		Narration:  "День 1. Утро.",
		Context:    "Без изменений.",
		Future:     "Вечером — совет.",
		Validation: "Лимит: 10/150.",
	}
	got := n.Render()
	assert.Contains(t, got, HeaderDialogue)
	assert.Contains(t, got, "День 1. Утро.")
	assert.Contains(t, got, HeaderContext)
	assert.Contains(t, got, "Без изменений.")
	assert.Contains(t, got, HeaderFuture)
	assert.Contains(t, got, "Вечером — совет.")
	assert.Contains(t, got, HeaderValidation)
	assert.Contains(t, got, "Лимит: 10/150.")
}
