package domain

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestStripRulesBlock_NoBlock(t *testing.T) {
	in := "**диалоги**\nПривет.\n\n**КОНТЕКСТ И ИЗМЕНЕНИЯ**\nstate: момент обновлён."
	assert.Equal(t, in, StripRulesBlock(in))
}

func TestStripRulesBlock_RemovesTrailingBlock(t *testing.T) {
	in := strings.Join([]string{
		"**диалоги и действия**",
		"Аньбу чуть наклонила голову.",
		"",
		"**КОНТЕКСТ И ИЗМЕНЕНИЯ**",
		"state.md: допрос.",
		"",
		"**ВАЛИДАЦИЯ ПРАВИЛ**",
		"- Лимит слов: 171 / 350",
		"- Управлял персонажем игрока: нет",
		"- NPC знал только то, что должен: да",
	}, "\n")
	out := StripRulesBlock(in)
	assert.NotContains(t, out, "ВАЛИДАЦИЯ ПРАВИЛ")
	assert.NotContains(t, out, "Лимит слов")
	assert.Contains(t, out, "**диалоги и действия**")
	assert.Contains(t, out, "**КОНТЕКСТ И ИЗМЕНЕНИЯ**")
}

func TestStripRulesBlock_CollapsesTrailingBlankLines(t *testing.T) {
	in := "**диалоги**\nx\n\n**КОНТЕКСТ**\ny\n\n\n\n**ВАЛИДАЦИЯ ПРАВИЛ**\n- a\n- b\n"
	out := StripRulesBlock(in)
	assert.False(t, strings.HasSuffix(out, "\n\n\n"), "trailing blank lines should be collapsed: %q", out)
	assert.NotContains(t, out, "ВАЛИДАЦИЯ ПРАВИЛ")
}

func TestStripRulesBlock_EmptyInput(t *testing.T) {
	assert.Equal(t, "", StripRulesBlock(""))
}

func TestStripRulesBlock_NotAnchoredInsideText(t *testing.T) {
	// "ВАЛИДАЦИЯ ПРАВИЛ" appearing in the middle of a quoted NPC
	// line must not be stripped — the regex is line-anchored and
	// requires the header to be on its own line.
	in := "**диалоги**\n— сказал ВАЛИДАЦИЯ ПРАВИЛ и ушёл.\n\n**КОНТЕКСТ**\nok\n"
	out := StripRulesBlock(in)
	assert.Contains(t, out, "ВАЛИДАЦИЯ ПРАВИЛ и ушёл")
}

func TestStripRulesBlock_OnlyRulesBlock(t *testing.T) {
	in := "**ВАЛИДАЦИЯ ПРАВИЛ**\n- Лимит слов: 50 / 350\n"
	out := StripRulesBlock(in)
	assert.Equal(t, "", out)
}

func TestStripRulesBlock_NoTrailingNewline(t *testing.T) {
	// Some LLM outputs omit the final newline. The strip must
	// still work and not leave a stray newline behind.
	in := "**диалоги**\nx\n\n**КОНТЕКСТ**\ny\n**ВАЛИДАЦИЯ ПРАВИЛ**\n- a"
	out := StripRulesBlock(in)
	assert.Equal(t, "**диалоги**\nx\n\n**КОНТЕКСТ**\ny", out)
}
