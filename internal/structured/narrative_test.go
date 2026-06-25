package structured

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const fullJSON = `{
  "narration": "Хината вздрогнула, отступила. Уши — алые.",
  "context": "state.md обновлён; update_npc: Хината — статус: смущена",
  "future": "Компания доберётся до Ичираку, обед",
  "validation": "Лимит слов: 80/150, NPC isolation: ок"
}`

func TestParse_OK(t *testing.T) {
	t.Parallel()
	n, err := Parse(fullJSON)
	require.NoError(t, err)
	assert.Equal(t, "Хината вздрогнула, отступила. Уши — алые.", n.Narration)
	assert.Equal(t, "state.md обновлён; update_npc: Хината — статус: смущена", n.Context)
	assert.Equal(t, "Компания доберётся до Ичираку, обед", n.Future)
	assert.Equal(t, "Лимит слов: 80/150, NPC isolation: ок", n.Validation)
}

func TestParse_StripsFence(t *testing.T) {
	t.Parallel()
	fenced := "```json\n" + fullJSON + "\n```"
	n, err := Parse(fenced)
	require.NoError(t, err)
	assert.Equal(t, "Хината вздрогнула, отступила. Уши — алые.", n.Narration)
}

func TestParse_StripsFenceNoLang(t *testing.T) {
	t.Parallel()
	fenced := "```\n" + fullJSON + "\n```"
	n, err := Parse(fenced)
	require.NoError(t, err)
	assert.Equal(t, "Хината вздрогнула, отступила. Уши — алые.", n.Narration)
}

func TestParse_NotJSON(t *testing.T) {
	t.Parallel()
	_, err := Parse("**диалоги и действия**\n— Хината вздрогнула...")
	assert.ErrorIs(t, err, ErrNotJSON)
}

func TestParse_Empty(t *testing.T) {
	t.Parallel()
	_, err := Parse("")
	assert.ErrorIs(t, err, ErrNotJSON)
}

func TestParse_Whitespace(t *testing.T) {
	t.Parallel()
	_, err := Parse("   \n\t  ")
	assert.ErrorIs(t, err, ErrNotJSON)
}

func TestParse_InvalidJSON(t *testing.T) {
	t.Parallel()
	// Looks like JSON (starts with '{') but is malformed.
	// Must NEVER return ErrNotJSON (that triggers markdown
	// fallback).  With jsonrepair the string may be fixed
	// and err == nil — that is acceptable.
	// "invalid" without quotes is harder: jsonrepair returns
	// error because it cannot guess the missing quotes.
	_, err := Parse(`{"narration": "ok", invalid}`)
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrNotJSON)
}

func TestLooksLikeJSON(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want bool
	}{
		{`{"a":1}`, true},
		{"  \n  {\"a\":1}", true},
		{"```json\n{}\n```", true},
		{"**диалоги**", false},
		{"", false},
		{"hello", false},
		{"  hello", false},
		{"Prefix text before JSON:\n{\"narration\":\"x\"}", true},
		{"Нужно записать это.\n```json\n{\"narration\":\"x\"}\n```", true},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, LooksLikeJSON(tc.in), "input=%q", tc.in)
	}
}

func TestMissingFields(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		body    string
		missing []string
	}{
		{
			name:    "all present",
			body:    fullJSON,
			missing: nil,
		},
		{
			name:    "future empty",
			body:    `{"narration":"x","context":"y","future":"  ","validation":"z"}`,
			missing: []string{"future"},
		},
		{
			name:    "all empty",
			body:    `{"narration":"","context":"","future":"","validation":""}`,
			missing: []string{"narration", "context", "future", "validation"},
		},
		{
			name:    "context missing entirely",
			body:    `{"narration":"x","future":"y","validation":"z"}`,
			missing: []string{"context"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			n, err := Parse(tc.body)
			require.NoError(t, err)
			assert.Equal(t, tc.missing, n.MissingFields())
		})
	}
}

func TestRender_4Blocks(t *testing.T) {
	t.Parallel()
	n, err := Parse(fullJSON)
	require.NoError(t, err)
	out := n.Render()
	// Must contain all 4 expected headers.
	for _, h := range []string{
		"**диалоги и действия**",
		"**КОНТЕКСТ И ИЗМЕНЕНИЯ**",
		"**БУДУЩЕЕ**",
		"**ВАЛИДАЦИЯ ПРАВИЛ**",
	} {
		assert.Contains(t, out, h, "missing header %q in:\n%s", h, out)
	}
	// Order matters — block 1 must precede block 4.
	idx1 := strings.Index(out, "**диалоги и действия**")
	idx4 := strings.Index(out, "**ВАЛИДАЦИЯ ПРАВИЛ**")
	assert.Less(t, idx1, idx4, "block order broken")
	// All four narratives must be present.
	assert.Contains(t, out, "Хината вздрогнула, отступила.")
	assert.Contains(t, out, "state.md обновлён")
	assert.Contains(t, out, "Компания доберётся")
	assert.Contains(t, out, "Лимит слов: 80/150")
}

func TestRender_TrimsTrailingWhitespace(t *testing.T) {
	t.Parallel()
	n := &Narrative{
		Narration:  "  сцена.  ",
		Context:    "  без изменений.\n\n  ",
		Future:     " обед  ",
		Validation: " лимит: 90/150  ",
	}
	out := n.Render()
	// Each block's content should be trimmed of leading
	// and trailing whitespace. TrimSpace collapses
	// internal runs too, so we only assert the
	// canonical clean form: "сцена." (the trailing
	// double space inside the string is collapsed by
	// TrimSpace, which is fine for our purposes).
	assert.True(t, strings.HasPrefix(out, "**диалоги и действия**\nсцена."),
		"got: %q", out)
	// No trailing whitespace on the last line.
	assert.False(t, strings.HasSuffix(out, " \n"), "got: %q", out)
	assert.False(t, strings.HasSuffix(out, "  "), "got: %q", out)
}

func TestParse_DuplicateJSON(t *testing.T) {
	t.Parallel()
	duped := fullJSON + "\n``````json\n" + fullJSON
	n, err := Parse(duped)
	require.NoError(t, err)
	assert.Equal(t, "Хината вздрогнула, отступила. Уши — алые.", n.Narration)
	assert.Equal(t, "state.md обновлён; update_npc: Хината — статус: смущена", n.Context)
}

func TestParse_DuplicateRawJSON(t *testing.T) {
	t.Parallel()
	duped := fullJSON + "\n" + fullJSON
	n, err := Parse(duped)
	require.NoError(t, err)
	assert.Equal(t, "Хината вздрогнула, отступила. Уши — алые.", n.Narration)
}

func TestParse_PrefixTextBeforeJSON(t *testing.T) {
	t.Parallel()
	prefixed := "Нужно записать это как действие.\n" + fullJSON
	n, err := Parse(prefixed)
	require.NoError(t, err)
	assert.Equal(t, "Хината вздрогнула, отступила. Уши — алые.", n.Narration)
}

func TestParse_PrefixAndFencedDuplicate(t *testing.T) {
	t.Parallel()
	prefixed := "Нужно записать это.\n```json\n" + fullJSON + "\n```\n``````json\n" + fullJSON + "\n```"
	n, err := Parse(prefixed)
	require.NoError(t, err)
	assert.Equal(t, "Хината вздрогнула, отступила. Уши — алые.", n.Narration)
}

func TestParse_InnerQuotes(t *testing.T) {
	t.Parallel()
	// Model uses '"' as typographic quotes inside a value
	broken := `{
  "narration": "Мизуки... \"Другой способ\"...",
  "context": "x",
  "future": "y",
  "validation": "z"
}`
	n, err := Parse(broken)
	require.NoError(t, err)
	assert.Equal(t, `Мизуки... "Другой способ"...`, n.Narration)
}

func TestParse_InnerQuotesWithChevrons(t *testing.T) {
	t.Parallel()
	// Real-world bug: model writes «"Другой способ"...»
	broken := `{
  "narration": "Мизуки... «\"Другой способ\"...»",
  "context": "x",
  "future": "y",
  "validation": "z"
}`
	n, err := Parse(broken)
	require.NoError(t, err)
	assert.Equal(t, `Мизуки... «"Другой способ"...»`, n.Narration)
}

func TestSanitizeJSONQuotes(t *testing.T) {
	t.Parallel()
	assert.Equal(t, `"a"`, string(sanitizeJSONQuotes([]byte(`"a"`))))
	assert.Equal(t, `"a\"b"`, string(sanitizeJSONQuotes([]byte(`"a"b"`))))
	assert.Equal(t, `"a\"b\"c"`, string(sanitizeJSONQuotes([]byte(`"a"b"c"`))))
	// valid JSON with structural neighbours — untouched
	assert.Equal(t, `"a","b"`, string(sanitizeJSONQuotes([]byte(`"a","b"`))))
	assert.Equal(t, `"a": "b\"c"`, string(sanitizeJSONQuotes([]byte(`"a": "b"c"`))))
	// real JSON object — structural quotes untouched
	payload := `{"narration": "x", "context": "y"}`
	assert.Equal(t, payload, string(sanitizeJSONQuotes([]byte(payload))))
}
