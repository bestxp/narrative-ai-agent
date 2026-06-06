package telegram

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMarkdownToHTML_Bold(t *testing.T) {
	assert.Equal(t, "<b>hello</b>", markdownToHTML("**hello**"))
}

func TestMarkdownToHTML_Italic(t *testing.T) {
	assert.Equal(t, "<i>hello</i>", markdownToHTML("*hello*"))
}

func TestMarkdownToHTML_Code(t *testing.T) {
	out := markdownToHTML("x`a <b>`y")
	assert.Contains(t, out, "<code>a &lt;b&gt;</code>")
}

func TestMarkdownToHTML_Link(t *testing.T) {
	out := markdownToHTML("start [label](https://e.com) end")
	assert.Contains(t, out, `<a href="https://e.com">label</a>`)
}

func TestMarkdownToHTML_BlockHeaders(t *testing.T) {
	in := "**диалоги и действия**\n- Hi\n\n**КОНТЕКСТ И ИЗМЕНЕНИЯ**\nok"
	out := markdownToHTML(in)
	assert.Contains(t, out, "<b>диалоги и действия</b>")
	assert.Contains(t, out, "<b>КОНТЕКСТ И ИЗМЕНЕНИЯ</b>")
}

func TestMarkdownToHTML_StripsLiteralHTML(t *testing.T) {
	out := markdownToHTML("a <b> c & d")
	assert.False(t, strings.Contains(out, "<b>"),
		"raw <b> in source must not survive: %q", out)
	assert.Contains(t, out, "a")
	assert.Contains(t, out, "c")
	assert.Contains(t, out, "d")
}

func TestMarkdownToHTML_PlainTextIsPassthrough(t *testing.T) {
	assert.Equal(t, "hello world", markdownToHTML("hello world"))
}

func TestMarkdownToHTML_Empty(t *testing.T) {
	assert.Equal(t, "", markdownToHTML(""))
}

func TestMarkdownToHTML_Russian(t *testing.T) {
	out := markdownToHTML("**Саске** сказал *привет* и `ушел`.")
	assert.Equal(t, "<b>Саске</b> сказал <i>привет</i> и <code>ушел</code>.", out)
}

func TestMarkdownToHTML_LiteralAsterisksArithmetic(t *testing.T) {
	out := markdownToHTML("2 * 3 = 6")
	assert.Equal(t, "2 * 3 = 6", out)
}

func TestMarkdownToHTML_Strikethrough(t *testing.T) {
	out := markdownToHTML("~~old~~ stays")
	assert.Contains(t, out, "<s>old</s>")
}

func TestMarkdownToHTML_DoubleUnderscoreIsBold(t *testing.T) {
	// Library matches Telegram semantics: __X__ is bold, not underline.
	// This mirrors what users see in the official Telegram app.
	out := markdownToHTML("__new__ stays")
	assert.Contains(t, out, "<b>new</b>")
}

func TestMarkdownToHTML_FencedCodeWithLanguage(t *testing.T) {
	out := markdownToHTML("```go\nfunc x(){}\n```")
	assert.Contains(t, out, `<pre><code class="language-go">`)
	assert.Contains(t, out, "func x(){}")
}

func TestMarkdownToHTML_Blockquote(t *testing.T) {
	out := markdownToHTML("> quoted line")
	assert.Contains(t, out, "<blockquote>")
	assert.Contains(t, out, "quoted line")
}

func TestMarkdownToHTML_Lists(t *testing.T) {
	out := markdownToHTML("- a\n- b")
	assert.Contains(t, out, "a")
	assert.Contains(t, out, "b")
}

func TestMarkdownToHTML_EmojiSurrogatePair(t *testing.T) {
	out := markdownToHTML("**😀**")
	assert.Contains(t, out, "<b>")
	assert.Contains(t, out, "😀")
}

func TestIsMessageNotModified(t *testing.T) {
	assert.True(t, isMessageNotModified(errString("Bad Request: message is not modified: specified new message content and reply markup are exactly the same as a current content and reply markup of the message")))
	assert.False(t, isMessageNotModified(errString("network timeout")))
	assert.False(t, isMessageNotModified(nil))
}

type stringErr string

func (s stringErr) Error() string { return string(s) }
func errString(s string) error    { return stringErr(s) }
