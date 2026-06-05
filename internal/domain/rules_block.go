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
//
// The current narrative.md does not ask the model to add a
// fourth block, so "drop everything after the rules header" is
// the right policy. If a future prompt grows a fourth block
// (e.g. **Ближайшие события**) it will be lost when the rules
// block is stripped — that is by design, the operator opted in
// to the rules block being hidden.
func StripRulesBlock(text string) string {
	if text == "" {
		return text
	}
	loc := rulesBlockRe.FindStringIndex(text)
	if loc == nil {
		return text
	}
	out := text[:loc[0]]
	// Collapse 3+ consecutive newlines that the removal may have
	// left behind into a single blank line, then trim trailing
	// newlines so the reply does not pick up an ugly gap.
	for strings.Contains(out, "\n\n\n") {
		out = strings.ReplaceAll(out, "\n\n\n", "\n\n")
	}
	return strings.TrimRight(out, "\n")
}
