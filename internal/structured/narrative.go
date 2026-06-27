// Package structured parses GM responses that arrive as
// JSON instead of markdown. When the openai driver is wired
// with structured_output.mode="json_object" the provider
// returns a JSON object with four fields (narration,
// context, future, validation) and the bot renders those
// into the same Telegram-friendly 4-block markdown the
// legacy driver emits.
//
// The package is intentionally small: it detects, parses,
// and re-emits. No I/O, no LLM, no slowlog. The GM owns
// format compliance (re-prompts the model when JSON is
// missing) and the renderer in this package only sees
// content that already passed validation upstream.
package structured

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"strings"

	"github.com/kaptinlin/jsonrepair"
)

// Section header constants used in the rendered markdown
// output. These are the canonical Russian headers that
// appear in every GM reply, regardless of whether the
// response arrived as JSON or plain-text sections.
const (
	HeaderDialogue   = "**ДИАЛОГИ И ДЕЙСТВИЯ**"
	HeaderContext    = "**КОНТЕКСТ И ИЗМЕНЕНИЯ**"
	HeaderFuture     = "**БУДУЩЕЕ**"
	HeaderValidation = "**ВАЛИДАЦИЯ ПРАВИЛ**"
)

// Narrative is the canonical 4-field shape the model
// emits in JSON mode. Field tags mirror what we tell the
// model in narrative.md (Режим A) — renaming them here
// means renaming them in the prompt AND in the render
// function in one place.
type Narrative struct {
	Narration  string `json:"narration"`
	Context    string `json:"context"`
	Future     string `json:"future"`
	Validation string `json:"validation"`
}

// ErrNotJSON is returned by Parse when the input is not
// detectable as a JSON object. The GM treats this as
// "send the raw markdown to the player" without
// logging a spurious warning.
var ErrNotJSON = errors.New("structured: input is not a JSON object")

// Parse attempts to extract a Narrative from text. It
// first strips leading/trailing whitespace and the
// ```json ... ``` fence the model occasionally wraps
// the payload in (Ollama Cloud model behaviour — see
// the stripJSONFence helper in cmd/test-openapi). It
// then unmarshals the result.
//
// If the text is not a pure JSON object, Parse tries
// harder: it scans for the first '{' and attempts to
// extract the first complete JSON object from that
// point. This handles two common model pathologies:
//
//  1. Prefix text before JSON ("Here is the response:")
//  2. Duplicated JSON objects (model emits the same
//     object twice, sometimes with a fence between).
//
// The error is ErrNotJSON when the body is not a JSON
// object at all (no '{' found). Any other decode error
// is wrapped with the original input for slowlog /
// debugging.
func Parse(text string) (*Narrative, error) {
	body := stripFence(strings.TrimSpace(text))
	// RouterAPI/Anthropic thinking leak: strip XML-like tags
	// that appear after the JSON object.
	body = stripThinkingTags(body)
	if looksLikeJSON(body) { //nolint:nestif // intentional JSON detection nesting
		var n Narrative

		cleaned := SanitizeJSONQuotes(body)
		if err := json.Unmarshal(cleaned, &n); err == nil {
			return &n, nil
		}

		first := extractFirstJSONObject(cleaned)
		if first != nil {
			var n2 Narrative
			if err := json.Unmarshal(first, &n2); err == nil {
				return &n2, nil
			}
		}
		// Last resort: structural repair (missing commas, brackets, quotes)
		repaired, err := jsonrepair.Repair(string(cleaned))
		if err == nil {
			var n3 Narrative
			if err := json.Unmarshal([]byte(repaired), &n3); err == nil {
				return &n3, nil
			}
		}

		return nil, errors.New("structured: json.Unmarshal: input is not a valid Narrative object")
	}
	// The text does not start with '{', but may contain a
	// JSON object after some prefix text (model hallucination:
	// "Here is the response: {...}" or a duplicated fenced
	// block). Try to find the first '{' and extract.
	idx := findOpeningBrace(body)
	if idx < 0 {
		return nil, ErrNotJSON
	}

	sub := body[idx:]

	var n Narrative

	cleaned := SanitizeJSONQuotes(sub)
	if err := json.Unmarshal(cleaned, &n); err == nil {
		return &n, nil
	}

	first := extractFirstJSONObject(cleaned)
	if first != nil {
		var n2 Narrative
		if err := json.Unmarshal(first, &n2); err == nil {
			return &n2, nil
		}
	}
	// Last resort: structural repair
	repaired, err := jsonrepair.Repair(string(cleaned))
	if err == nil {
		var n3 Narrative
		if err := json.Unmarshal([]byte(repaired), &n3); err == nil {
			return &n3, nil
		}
	}

	return nil, ErrNotJSON
}

// StripThinkingTags removes RouterAPI/Anthropic thinking leak
// artifacts: XML-like tags that appear after the JSON payload.
// Examples: <arg_key>...</arg_key>, </arg_value>.</tool_call>.
func StripThinkingTags(data string) string {
	b := []byte(data)
	// Find the first occurrence of </arg_value> or <arg_key>
	// and truncate everything from that point.
	idx := bytes.Index(b, []byte("</arg_value>"))
	if idx >= 0 {
		return strings.TrimSpace(string(b[:idx]))
	}

	idx = bytes.Index(b, []byte("<arg_key>"))
	if idx >= 0 {
		return strings.TrimSpace(string(b[:idx]))
	}

	return data
}

func stripThinkingTags(data []byte) []byte {
	return []byte(StripThinkingTags(string(data)))
}

func findOpeningBrace(data []byte) int {
	inStr := false
	escape := false

	for i := range data {
		c := data[i]

		if escape {
			escape = false

			continue
		}

		if c == '\\' && inStr {
			escape = true

			continue
		}

		if c == '"' {
			inStr = !inStr

			continue
		}

		if !inStr && c == '{' {
			return i
		}
	}

	return -1
}

func extractFirstJSONObject(data []byte) []byte {
	start := -1
	depth := 0
	inStr := false
	escape := false

	for i := range data {
		c := data[i]

		if escape {
			escape = false

			continue
		}

		if c == '\\' && inStr {
			escape = true

			continue
		}

		if c == '"' {
			inStr = !inStr

			continue
		}

		if inStr {
			continue
		}

		switch c {
		case '{':
			if start < 0 {
				start = i
			}

			depth++
		case '}':
			depth--
			if depth == 0 && start >= 0 {
				return data[start : i+1]
			}
		}
	}

	return nil
}

// SanitizeJSONQuotes escapes unescaped double quotes that appear
// inside JSON string literals. Models occasionally use '"' as
// typographic quotation marks inside narration or dialogue (e.g.
// «"Другой способ"...»); these break the JSON parser because
// the parser treats the first '"' as the string terminator.
//
// The heuristic: a '"' that is surrounded by non-structural
// characters (not preceded/followed by JSON structural chars)
// is an inner quote and gets escaped as '\"'. We preserve
// opening and closing quotes that sit next to structural chars.
// When we escape an inner quote we do NOT toggle the in-string
// state — the string stays open.
func SanitizeJSONQuotes(data []byte) []byte {
	var out bytes.Buffer

	inStr := false

	escape := false
	for i, c := range data {
		if escape {
			out.WriteByte(c)

			escape = false

			continue
		}

		if c == '\\' && inStr {
			out.WriteByte(c)

			escape = true

			continue
		}

		if c == '"' {
			if inStr && !isJSONStructural(data, i-1) && !isJSONStructural(data, i+1) {
				// Inner quote surrounded by text — escape it,
				// but STAY inside the string.
				out.WriteByte('\\')
				out.WriteByte(c)

				continue
			}

			out.WriteByte(c)

			inStr = !inStr

			continue
		}

		out.WriteByte(c)
	}

	return out.Bytes()
}

func isJSONStructural(data []byte, idx int) bool {
	if idx < 0 || idx >= len(data) {
		return true // boundary counts as structural
	}

	switch data[idx] {
	case '{', '}', '[', ']', ':', ',', '\n', '\r', '\t', ' ':
		return true
	}

	return false
}

// LooksLikeJSON is a fast pre-check used by the GM to
// decide whether to invoke Parse at all. We accept any
// text that contains a '{' outside a string literal —
// this catches prefix text before JSON ("Here is the
// response: {") as well as pure JSON ("{...}").
func LooksLikeJSON(text string) bool {
	return findOpeningBrace(stripFence(strings.TrimSpace(text))) >= 0
}

// looksLikeJSON after fence strip. Empty bodies and
// bodies that start with anything other than '{' (e.g.
// a markdown header "**диалоги**") are not JSON.
func looksLikeJSON(body []byte) bool {
	for _, c := range body {
		switch c {
		case ' ', '\t', '\n', '\r':
			continue
		case '{':
			return true
		default:
			return false
		}
	}

	return false
}

// stripFence removes a leading ```json (or ```) and the
// matching trailing ```. Some Ollama Cloud models wrap
// JSON in code fences even when response_format is set;
// we still want to parse the body.
func stripFence(s string) []byte {
	if !strings.HasPrefix(s, "```") {
		return []byte(s)
	}

	if idx := strings.Index(s, "\n"); idx > 0 {
		s = s[idx+1:]
	}

	if idx := strings.LastIndex(s, "```"); idx >= 0 {
		s = s[:idx]
	}

	return []byte(strings.TrimSpace(s))
}

// MissingFields reports which of the four required
// fields are absent or empty. The GM uses this to drive
// the format re-prompt: a missing field is the same
// kind of contract violation as a missing markdown
// header in the legacy path.
func (n *Narrative) MissingFields() []string {
	var missing []string
	if strings.TrimSpace(n.Narration) == "" {
		missing = append(missing, "narration")
	}

	if strings.TrimSpace(n.Context) == "" {
		missing = append(missing, "context")
	}

	if strings.TrimSpace(n.Future) == "" {
		missing = append(missing, "future")
	}

	if strings.TrimSpace(n.Validation) == "" {
		missing = append(missing, "validation")
	}

	return missing
}

// RenderPartial renders only the sections that have already
// received non-empty content. This is meant for live streaming
// UIs where empty placeholder sections would otherwise flash
// in the interface while the response is still being generated.
func (n *Narrative) RenderPartial() string {
	var b strings.Builder
	first := true

	writeSection := func(header, body string) {
		if strings.TrimSpace(body) == "" {
			return
		}

		if !first {
			b.WriteString("\n\n")
		}
		first = false

		b.WriteString(header)
		b.WriteByte('\n')
		b.WriteString(strings.TrimSpace(body))
	}

	writeSection(HeaderDialogue, n.Narration)
	writeSection(HeaderContext, n.Context)
	writeSection(HeaderFuture, n.Future)
	writeSection(HeaderValidation, n.Validation)

	if b.Len() == 0 {
		return ""
	}

	b.WriteByte('\n')

	return b.String()
}

// Render converts a parsed Narrative into the 4-block
// markdown the legacy path emits. The output is byte-
// for-byte identical to what a well-behaved markdown
// reply would produce for the same content, modulo
// the exact phrasing of the context/future lines
// (which the model controls in both modes).
//
// The function does not strip or massage the model
// output beyond trimming trailing whitespace per
// block. Any operator-facing text rules (max tokens,
// ВАЛИДАЦИЯ ПРАВИЛ visibility, etc.) are applied
// downstream by main.go's stripRules helper, the
// same way they are applied to the markdown path.
// section marker names as they appear inside "------ ... ------".
const (
	sectionNarrative = "NARRATIVE"
	sectionContext   = "CONTEXT"
	sectionFuture    = "FUTURE"
	sectionSystem    = "SYSTEM"
	sectionEnd       = "END"
)

// markerRe matches a "------ NAME ------" section marker anywhere in
// the text. It tolerates whitespace around the name and captures
// the name (group 1) so it can be normalised and dispatched. Markers
// may appear on their own line or inline after section text.
var markerRe = regexp.MustCompile(`(?i)------\s*([A-Za-z][A-Za-z0-9_\-]*)\s*------`)

// ParsePlain extracts a Narrative from a section-delimited
// plain-text response. The format is:
//
//	------ NARRATIVE ------
//	...narration text...
//	------ CONTEXT ------
//	...context text...
//	------ FUTURE ------
//	...future text...
//	------ SYSTEM ------
//	...validation text...
//	------ END ------
//
// Section markers are case-insensitive and tolerate leading/trailing
// whitespace. They may also appear on the same line as the preceding
// section text. Text between markers is trimmed. Missing sections
// default to empty strings. The function is idempotent: calling it
// on a partial stream returns whatever sections have been fully
// received so far.
func ParsePlain(text string) (*Narrative, error) {
	return parseSectionPlain(text)
}

// RenderAny normalizes either a JSON or a plain section-delimited
// response into the canonical bold Russian headers. It tries JSON
// first; if that fails it tries plain markers; if both fail it
// returns the raw text untouched. This is the single function every
// transport should use for final replies so the user sees the same
// format regardless of the LLM output mode.
func RenderAny(text string) string {
	clean := StripThinkingTags(text)
	if LooksLikeJSON(clean) {
		n, err := Parse(clean)
		if err == nil {
			return n.Render()
		}
	}

	n, err := ParsePlain(clean)
	if err == nil {
		return n.Render()
	}

	return clean
}

// RenderAnyPartial is the streaming equivalent of RenderAny: it
// renders only the sections that have already received content,
// omitting empty placeholders. Use it in live-update callbacks
// (OnDelta / streaming Append) so the player does not see four
// blank sections while the response is still being generated.
func RenderAnyPartial(text string) string {
	clean := StripThinkingTags(text)
	if LooksLikeJSON(clean) {
		n, err := Parse(clean)
		if err == nil {
			return n.RenderPartial()
		}
	}

	n, err := ParsePlain(clean)
	if err == nil {
		return n.RenderPartial()
	}

	return clean
}

// parseSectionPlain is the raw section-marker parser used by ParsePlain.
func parseSectionPlain(text string) (*Narrative, error) {
	var n Narrative

	// Find all markers in order. We split the text into slices
	// between markers, preserving any trailing text after the last
	// marker seen so far.
	matches := markerRe.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		return &n, nil
	}

	for i, m := range matches {
		name := strings.ToUpper(strings.TrimSpace(text[m[2]:m[3]]))
		start := m[1] // content starts after the matched marker

		var end int
		if i+1 < len(matches) {
			end = matches[i+1][0] // next marker begins here
		} else {
			end = len(text) // no more markers, take rest of text
		}

		val := strings.TrimSpace(text[start:end])

		switch name {
		case sectionNarrative:
			n.Narration = val
		case sectionContext:
			n.Context = val
		case sectionFuture:
			n.Future = val
		case sectionSystem:
			n.Validation = val
		case sectionEnd:
			return &n, nil
		}
	}

	return &n, nil
}

func (n *Narrative) Render() string {
	var b strings.Builder

	b.WriteString(HeaderDialogue)
	b.WriteByte('\n')
	b.WriteString(strings.TrimSpace(n.Narration))
	b.WriteString("\n\n")

	b.WriteString(HeaderContext)
	b.WriteByte('\n')
	b.WriteString(strings.TrimSpace(n.Context))
	b.WriteString("\n\n")

	b.WriteString(HeaderFuture)
	b.WriteByte('\n')
	b.WriteString(strings.TrimSpace(n.Future))
	b.WriteString("\n\n")

	b.WriteString(HeaderValidation)
	b.WriteByte('\n')
	b.WriteString(strings.TrimSpace(n.Validation))
	b.WriteByte('\n')

	return b.String()
}
