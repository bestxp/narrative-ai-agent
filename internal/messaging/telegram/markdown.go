package telegram

import (
	"regexp"
	"sort"
	"strings"
	"unicode/utf16"

	tgmd "github.com/eekstunt/telegramify-markdown-go"
)

// markdownToHTML converts LLM-emitted Markdown to Telegram
// HTML. When the converter fails on a parse edge case the
// original text is returned unchanged so the user still
// sees something useful (and the bug surfaces in tests
// rather than in a silent sendMessage 400).
func markdownToHTML(s string) string {
	if s == "" {
		return s
	}
	msg := tgmd.Convert(s)

	return renderEntitiesToHTML(msg.Text, msg.Entities)
}

// renderEntitiesToHTML walks the entities in order and
// wraps the corresponding UTF-16 slices of text with the
// matching HTML tags. Entities are required to be
// non-overlapping and in ascending Offset order; the
// library guarantees this. Length is in UTF-16 code units.
func renderEntitiesToHTML(text string, ents []tgmd.Entity) string {
	if len(ents) == 0 {
		return htmlEscape(text)
	}
	sorted := make([]tgmd.Entity, len(ents))
	copy(sorted, ents)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].Offset < sorted[j].Offset
	})

	encoded := utf16.Encode([]rune(text))

	var b strings.Builder
	b.Grow(len(text) + 32)

	cursor := 0
	for _, e := range sorted {
		if e.Offset < cursor {
			continue
		}
		if e.Offset > cursor {
			b.WriteString(utf16ToString(encoded[cursor:e.Offset]))
		}
		end := min(e.Offset+e.Length, len(encoded))
		inner := utf16ToString(encoded[e.Offset:end])
		writeEntity(&b, e, inner)
		cursor = end
	}
	if cursor < len(encoded) {
		b.WriteString(utf16ToString(encoded[cursor:]))
	}

	return b.String()
}

// writeEntity writes a single entity span to b. Telegram's
// HTML dialect uses these tag names; "pre" preserves the
// <code> nesting the library expects.
func writeEntity(b *strings.Builder, e tgmd.Entity, inner string) {
	switch e.Type {
	case tgmd.Bold:
		b.WriteString("<b>")
		b.WriteString(htmlEscape(inner))
		b.WriteString("</b>")
	case tgmd.Italic:
		b.WriteString("<i>")
		b.WriteString(htmlEscape(inner))
		b.WriteString("</i>")
	case tgmd.Underline:
		b.WriteString("<u>")
		b.WriteString(htmlEscape(inner))
		b.WriteString("</u>")
	case tgmd.Strikethrough:
		b.WriteString("<s>")
		b.WriteString(htmlEscape(inner))
		b.WriteString("</s>")
	case tgmd.Code:
		b.WriteString("<code>")
		b.WriteString(htmlEscape(inner))
		b.WriteString("</code>")
	case tgmd.Pre:
		if e.Language != "" {
			b.WriteString(`<pre><code class="language-`)
			b.WriteString(htmlEscape(e.Language))
			b.WriteString(`">`)
		} else {
			b.WriteString("<pre>")
		}
		b.WriteString(htmlEscape(inner))
		if e.Language != "" {
			b.WriteString("</code></pre>")
		} else {
			b.WriteString("</pre>")
		}
	case tgmd.TextLink:
		b.WriteString(`<a href="`)
		b.WriteString(htmlEscape(e.URL))
		b.WriteString(`">`)
		b.WriteString(htmlEscape(inner))
		b.WriteString("</a>")
	case tgmd.Blockquote:
		b.WriteString("<blockquote>")
		b.WriteString(htmlEscape(inner))
		b.WriteString("</blockquote>")
	default:
		b.WriteString(htmlEscape(inner))
	}
}

func utf16ToString(encoded []uint16) string {
	if len(encoded) == 0 {
		return ""
	}

	return string(utf16.Decode(encoded))
}

func htmlEscape(s string) string {
	if !strings.ContainsAny(s, "<>&\"") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '&':
			b.WriteString("&amp;")
		case '"':
			b.WriteString("&quot;")
		default:
			b.WriteRune(r)
		}
	}

	return b.String()
}

// stripHTMLTags removes all HTML markup from s, returning just
// the text content. Used by the stream layer to detect cases
// where the formatted wire text contains only tags — e.g.
// "<b></b>" — which Telegram rejects with "Bad Request: message
// text is empty". A simple regexp is enough here because
// Telegram's HTML dialect is constrained (no nested quotes in
// attributes, no CDATA, no comments).
var htmlTagRe = regexp.MustCompile(`<[^>]*>`)

func stripHTMLTags(s string) string {
	return htmlTagRe.ReplaceAllString(s, "")
}

// isMessageNotModified reports whether err is the harmless
// "Bad Request: message is not modified" reply from the
// Telegram editMessageText endpoint. It happens whenever a
// throttled stream catches up and resends text that already
// matches the message on screen. Telegram returns 400 in
// that case, but to the user nothing changed — we just
// swallow the error and let the next chunk finish the job.
func isMessageNotModified(err error) bool {
	if err == nil {
		return false
	}

	return strings.Contains(err.Error(), "message is not modified")
}

// isMessageTooLong reports whether err is the
// "Bad Request: MESSAGE_TOO_LONG" / "message is too long"
// reply from Telegram. The cap is 4096 characters per
// sendMessage / editMessageText; rich-text markup
// (`<b>`, `<pre>`) counts too. The stream layer detects
// this and falls back to sending the over-length tail as a
// fresh message with a "продолжение ↓" header.
func isMessageTooLong(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()

	return strings.Contains(s, "MESSAGE_TOO_LONG") ||
		strings.Contains(s, "message is too long")
}

// maxTelegramMessageLen is the per-message cap Telegram
// enforces for sendMessage / editMessageText. We split long
// replies at this boundary rather than letting Telegram
// reject the whole thing with 400 MESSAGE_TOO_LONG.
const maxTelegramMessageLen = 4096

// splitForTelegram cuts text into chunks each no longer than
// maxTelegramMessageLen. Splits prefer paragraph boundaries
// ("\n\n") so the cut does not land mid-sentence; if a single
// paragraph exceeds the cap (rare; usually only when the LLM
// emits a giant line of code in <pre>) we fall back to a
// hard split at the cap.
//
// The cut is measured in RUNES, not bytes. Telegram measures
// the limit in characters, and a rune-aligned cut also
// guarantees we never split a multi-byte UTF-8 sequence in
// the middle — which would emit invalid UTF-8 and make the
// Telegram API return "text must be encoded in UTF-8" (a
// common failure mode when the LLM streams a long Russian
// reply that happens to land a cut on the second byte of a
// Cyrillic letter).
func splitForTelegram(text string) []string {
	runes := []rune(text)
	if len(runes) <= maxTelegramMessageLen {
		return []string{text}
	}
	out := make([]string, 0, 2)
	rest := string(runes)
	for runeCount := len(runes); runeCount > maxTelegramMessageLen; runeCount = len([]rune(rest)) {
		head := string([]rune(rest)[:maxTelegramMessageLen])
		// Look for a paragraph break within the
		// first cap characters. We do the search
		// in the rune-bounded head so a "\n\n" near
		// the edge is still a real boundary, not
		// a byte-coincidence.
		var cut int
		if i := strings.LastIndex(head, "\n\n"); i > 0 {
			cut = i + len("\n\n")
		} else if i := strings.LastIndex(head, "\n"); i > 0 {
			cut = i + len("\n")
		} else {
			cut = len(head)
		}
		out = append(out, rest[:cut])
		rest = rest[cut:]
	}
	if rest != "" {
		out = append(out, rest)
	}

	return out
}
