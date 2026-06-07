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
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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
// The error is ErrNotJSON when the body is not a JSON
// object at all (no leading '{' or '{' preceded by
// a fence). Any other decode error is wrapped with
// the original input for slowlog / debugging.
func Parse(text string) (*Narrative, error) {
	body := stripFence(strings.TrimSpace(text))
	if !looksLikeJSON(body) {
		return nil, ErrNotJSON
	}
	var n Narrative
	if err := json.Unmarshal(body, &n); err != nil {
		return nil, fmt.Errorf("structured: json.Unmarshal: %w", err)
	}
	return &n, nil
}

// LooksLikeJSON is a fast pre-check used by the GM to
// decide whether to invoke Parse at all. We accept any
// text whose first non-whitespace character is '{'.
// The full parse happens lazily — this function only
// short-circuits the "obviously markdown" case.
func LooksLikeJSON(text string) bool {
	return looksLikeJSON(stripFence(strings.TrimSpace(text)))
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
