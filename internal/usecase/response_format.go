package usecase

import (
	"regexp"
	"strings"
)

// ResponseFormat enforces the "RESPONSE FORMAT" section of the skill.
// It is intentionally tiny — the real validation is the contract the
// renderer (Telegram adapter) must satisfy for every reply.
type ResponseFormat struct {
	WordLimit int
	Language  string
}

func NewResponseFormat(wordLimit int, language string) *ResponseFormat {
	if wordLimit == 0 {
		wordLimit = 350
	}
	if language == "" {
		language = "ru"
	}
	return &ResponseFormat{WordLimit: wordLimit, Language: language}
}

type Validation struct {
	HasDialogue       bool
	HasContextBlock   bool
	HasFutureBlock    bool
	HasValidationBlk  bool
	WordCount         int
	OverLimit         bool
	LatinOnly         bool
	ForbiddenForms    []string
}

// Validate checks structural compliance. It is intentionally lenient:
// "over limit" only reports a warning unless caller asks to enforce.
// The four block markers correspond to the four sections of the
// "RESPONSE FORMAT" rule in prompts/narrative.md:
//
//	**диалоги и действия**      — narrative prose
//	**КОНТЕКСТ И ИЗМЕНЕНИЯ**     — what files were touched
//	**БУДУЩЕЕ**                  — 1-2 lines from plan.md
//	**ВАЛИДАЦИЯ ПРАВИЛ**         — self-check
func (r *ResponseFormat) Validate(body string) Validation {
	v := Validation{
		WordCount: wordCount(body),
		LatinOnly: looksLikePlayerOutput(body),
	}
	if v.WordCount > r.WordLimit {
		v.OverLimit = true
	}
	v.HasDialogue = containsBlock(body, "**диалоги и действия**")
	v.HasContextBlock = containsBlock(body, "**КОНТЕКСТ И ИЗМЕНЕНИЯ**")
	v.HasFutureBlock = containsBlock(body, "**БУДУЩЕЕ**")
	v.HasValidationBlk = containsBlock(body, "**ВАЛИДАЦИЯ ПРАВИЛ**")
	v.ForbiddenForms = scanForbiddenForms(body)
	return v
}

var (
	cjkRe       = regexp.MustCompile(`[\p{Han}\p{Hiragana}\p{Katakana}]`)
	cyrillicRe  = regexp.MustCompile(`[\p{Cyrillic}]`)
)

// looksLikePlayerOutput returns true if the body is mostly latin —
// i.e. safe to persist without transliteration. Cyrillic is fine
// (Russian narrative), only CJK is flagged.
func looksLikePlayerOutput(s string) bool {
	return !cjkRe.MatchString(s)
}

// scanForbiddenForms flags any forbidden second-person constructions
// that the GM should not use. These come from skill rule #1.
var forbiddenForms = []string{
	"ты усмехнулся",
	"ты подумал",
	"ты почувствовал",
	"что делаем?",
	"куда идём?",
	"куда идем?",
}

func scanForbiddenForms(body string) []string {
	low := strings.ToLower(body)
	var hits []string
	for _, f := range forbiddenForms {
		if strings.Contains(low, f) {
			hits = append(hits, f)
		}
	}
	return hits
}

func containsBlock(body, marker string) bool {
	return strings.Contains(body, marker)
}

func wordCount(s string) int {
	// Strip CJK characters: each is a word in itself.
	cjk := cjkRe.FindAllString(s, -1)
	// Words from remaining text: split on whitespace.
	remaining := cjkRe.ReplaceAllString(s, " ")
	parts := strings.Fields(remaining)
	return len(parts) + len(cjk)
}

// ForbidNonLatinPath reports if any path in body looks cyrillic.
// Used by the maintenance check to catch leaked cyrillic filenames.
func ForbidNonLatinPath(body string) bool {
	return cyrillicRe.MatchString(body)
}
