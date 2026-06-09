package telegram

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

// TestIsMessageTooLong covers the two Telegram error
// phrasings for the 4096-char cap. The first is the
// short-form returned by the bot API; the second is the
// long-form some proxies (Cloudflare in front of older
// Telegram cores) use.
func TestIsMessageTooLong(t *testing.T) {
	assert.True(t, isMessageTooLong(errString("Bad Request: MESSAGE_TOO_LONG")))
	assert.True(t, isMessageTooLong(errString("Bad Request: message is too long")))
	assert.True(t, isMessageTooLong(errString("400 MESSAGE_TOO_LONG: chat is rate-limited")))
	assert.False(t, isMessageTooLong(errString("Bad Request: chat not found")))
	assert.False(t, isMessageTooLong(nil))
}

// TestSplitForTelegram_ShortText is the no-split case: text
// already fits, returned as a single chunk.
func TestSplitForTelegram_ShortText(t *testing.T) {
	out := splitForTelegram("hello world")
	assert.Equal(t, []string{"hello world"}, out)
}

// TestSplitForTelegram_AtParagraphBoundary covers the
// preferred path: text > 4096, but a "\n\n" within the cap
// marks a clean cut. Result is two chunks, each ≤ 4096.
func TestSplitForTelegram_AtParagraphBoundary(t *testing.T) {
	para1 := strings.Repeat("a", 3000)
	para2 := strings.Repeat("b", 3000)
	text := para1 + "\n\n" + para2
	out := splitForTelegram(text)
	require.Len(t, out, 2, "should split at the \\n\\n boundary, not later")
	assert.Equal(t, para1+"\n\n", out[0])
	assert.Equal(t, para2, out[1])
	for i, c := range out {
		assert.LessOrEqual(t, len(c), maxTelegramMessageLen, "chunk %d exceeds cap", i)
	}
}

// TestSplitForTelegram_ThreeChunks covers a longer text
// that requires three splits, each at the most recent
// paragraph break within the cap.
func TestSplitForTelegram_ThreeChunks(t *testing.T) {
	paras := []string{
		strings.Repeat("a", 3500),
		strings.Repeat("b", 3500),
		strings.Repeat("c", 3500),
	}
	text := strings.Join(paras, "\n\n")
	out := splitForTelegram(text)
	require.GreaterOrEqual(t, len(out), 2, "should split at least once")
	for i, c := range out {
		assert.LessOrEqual(t, len(c), maxTelegramMessageLen, "chunk %d exceeds cap", i)
	}
	reassembled := strings.Join(out, "")
	assert.Equal(t, text, reassembled, "splitting must round-trip cleanly")
}

// TestSplitForTelegram_HardCutOnGiantParagraph covers the
// rare case where one paragraph exceeds the cap on its
// own. The splitter falls back to a hard cut at the cap;
// the round-trip is still lossless.
func TestSplitForTelegram_HardCutOnGiantParagraph(t *testing.T) {
	big := strings.Repeat("x", maxTelegramMessageLen+500)
	out := splitForTelegram(big)
	require.GreaterOrEqual(t, len(out), 2)
	for i, c := range out {
		assert.LessOrEqual(t, len(c), maxTelegramMessageLen, "chunk %d exceeds cap", i)
	}
	assert.Equal(t, big, strings.Join(out, ""))
}

// TestSplitForTelegram_RussianAtCut is the regression
// guard for the "text must be encoded in UTF-8" Telegram
// error. A 2-byte Cyrillic letter split between its two
// bytes is invalid UTF-8. The splitter must align cuts
// to rune boundaries (not byte boundaries) so the chunk
// is always valid.
func TestSplitForTelegram_RussianAtCut(t *testing.T) {
	// Build a string of length just over the cap
	// where the cut point at maxTelegramMessageLen
	// would land inside a Cyrillic letter if measured
	// in bytes. Russian "К" is 0xD0 0x9A (2 bytes);
	// a string of length cap where every char is a
	// Russian letter has cap*2 bytes — the byte
	// cut at offset cap would land at byte 4096,
	// which is the middle of the 4097th byte (the
	// second byte of the 2049th Cyrillic letter).
	var b strings.Builder
	for b.Len() < maxTelegramMessageLen*2+100 {
		b.WriteString("Кагуя и Хината смотрят на Саске. ")
	}
	in := b.String()
	out := splitForTelegram(in)
	require.GreaterOrEqual(t, len(out), 2)
	for i, c := range out {
		// Each chunk must be valid UTF-8 (the
		// invariant Telegram cares about).
		assert.True(t, utf8.ValidString(c), "chunk %d is not valid UTF-8", i)
		// And the round-trip is lossless.
		assert.LessOrEqual(t, len([]rune(c)), maxTelegramMessageLen,
			"chunk %d exceeds rune cap", i)
	}
	assert.Equal(t, in, strings.Join(out, ""))
}

type stringErr string

func (s stringErr) Error() string { return string(s) }
func errString(s string) error    { return stringErr(s) }
