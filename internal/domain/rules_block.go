package domain

import (
	"regexp"
	"strings"
)

// rulesBlockRe matches the LLM-generated "**ВАЛИДАЦИЯ ПРАВИЛ**"
// header on its own line. The match is line-anchored at the
// start, so the phrase inside a quoted NPC line ("— сказал
// ВАЛИДАЦИЯ ПРАВИЛ ...") is not caught. We then drop everything
// from the match offset to end of string.
var rulesBlockRe = regexp.MustCompile(`(?m)^\*\*ВАЛИДАЦИЯ ПРАВИЛ\*\*\s*$`)

// StripRulesBlock removes the trailing "**ВАЛИДАЦИЯ ПРАВИЛ**"
// block from an LLM reply. The block is the last segment of the
// GM's reply (after **диалоги** and **КОНТЕКСТ И ИЗМЕНЕНИЯ**),
// so truncating at the rules header is enough — the bullet list
// that follows is dropped along with any further headers the
// LLM might have appended.
func StripRulesBlock(text string) string {
	if text == "" {
		return text
	}
	loc := rulesBlockRe.FindStringIndex(text)
	if loc == nil {
		return text
	}
	out := text[:loc[0]]
	return cleanTrailingWhitespace(out)
}

func cleanTrailingWhitespace(s string) string {
	for strings.Contains(s, "\n\n\n") {
		s = strings.ReplaceAll(s, "\n\n\n", "\n\n")
	}
	return strings.TrimRight(s, "\n ")
}
