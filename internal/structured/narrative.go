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
	"fmt"
	"strings"

	"github.com/kaptinlin/jsonrepair"
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
// detectable as a JSON object. The GM uses this sentinel
// to fall back to the legacy markdown parser without
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
	if looksLikeJSON(body) {
		var n Narrative
		cleaned := sanitizeJSONQuotes(body)
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
		return nil, fmt.Errorf("structured: json.Unmarshal: input is not a valid Narrative object")
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
	cleaned := sanitizeJSONQuotes(sub)
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
// Examples: <arg_key>...</arg_key>, </arg_value></tool_call>
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
	for i := 0; i < len(data); i++ {
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
	for i := 0; i < len(data); i++ {
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
		if c == '{' {
			if start < 0 {
				start = i
			}
			depth++
		} else if c == '}' {
			depth--
			if depth == 0 && start >= 0 {
				return data[start : i+1]
			}
		}
	}
	return nil
}

// sanitizeJSONQuotes escapes unescaped double quotes that appear
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
func sanitizeJSONQuotes(data []byte) []byte {
	var out bytes.Buffer
	inStr := false
	escape := false
	for i := 0; i < len(data); i++ {
		c := data[i]
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
func (n *Narrative) Render() string {
	var b strings.Builder
	// Block 1: narration under the bold "диалоги и
	// действия" header. The model uses this exact
	// Russian phrasing in markdown mode too; we keep
	// it identical so the player sees the same
	// visual block whether the reply arrived as
	// JSON or markdown.
	b.WriteString("**диалоги и действия**\n")
	b.WriteString(strings.TrimSpace(n.Narration))
	b.WriteString("\n\n")
	// Block 2: context. Legacy uses the header
	// "КОНТЕКСТ И ИЗМЕНЕНИЯ" with a bulleted list
	// of file updates. The JSON mode gives us
	// free-form text — we drop the leading "- "
	// because there is no list to render, and the
	// model already wrote prose.
	b.WriteString("**КОНТЕКСТ И ИЗМЕНЕНИЯ**\n")
	b.WriteString(strings.TrimSpace(n.Context))
	b.WriteString("\n\n")
	// Block 3: future.
	b.WriteString("**БУДУЩЕЕ**\n")
	b.WriteString(strings.TrimSpace(n.Future))
	b.WriteString("\n\n")
	// Block 4: validation. Same shape as legacy —
	// 1-2 lines, model-controlled.
	b.WriteString("**ВАЛИДАЦИЯ ПРАВИЛ**\n")
	b.WriteString(strings.TrimSpace(n.Validation))
	b.WriteString("\n")
	return b.String()
}
