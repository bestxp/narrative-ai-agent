package charprofile

import (
	"errors"
	"fmt"
	"strings"
)

// ErrUnknownFile is returned by MigrateFromMarkdown
// when the legacy file is not one of the three
// supported types. The caller (firstlaunch /
// maintenance) should ignore the error and skip.
var ErrUnknownFile = errors.New("charprofile: unknown legacy file")

// MigrateFromMarkdown parses a legacy free-form
// markdown body into the corresponding YAML
// payload. This is the deterministic fallback used
// when the LLM-driven migration is unavailable
// (driver down, prompt unreadable, YAML parse
// failure on the LLM response).
//
// The heuristic is the same as npcprofile:
//
//	# <DisplayName>
//	## <section>
//	- value
//	- value
//	## <next section>
//	- value
//
// Numbered lists ("1. ", "2. ") are accepted. Any
// value that does not start with `- `, `* ` or
// `<digits>. ` is treated as raw text. Empty
// trailing lines are dropped.
//
// The "name" field of the resulting payload is
// taken from the legacy H1 if present, else from
// fileSlug. The "soul" field (Soul only) is left
// empty — the LLM-driven path is the right tool
// to fill it; a deterministic parser that guesses
// a soul line would just be wrong.
func MigrateFromMarkdown(kind string, body, fileSlug string) (any, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return nil, ErrNotFound
	}
	switch kind {
	case "SOUL":
		var s Soul
		s.Name = fileSlug
		parseMarkdownSections(body, &s.Base, false)
		return s, nil
	case "skill":
		var s Skill
		s.Name = fileSlug
		parseMarkdownSections(body, &s.Base, true)
		return s, nil
	case "memory":
		var m Memory
		m.Name = fileSlug
		parseMarkdownSections(body, &m.Base, true)
		return m, nil
	}
	return nil, fmt.Errorf("%w: %s", ErrUnknownFile, kind)
}

// parseMarkdownSections walks the body line-by-line
// and populates b.Data. The strict flag controls
// whether unknown section names are dropped (Soul)
// or kept (Skill / Memory get their canonical
// sections; non-canonical names are folded into
// the "Прочее" section on Soul, dropped on
// Skill / Memory).
func parseMarkdownSections(body string, b *Base, strict bool) {
	lines := strings.Split(body, "\n")
	var current *Section
	for _, raw := range lines {
		t := strings.TrimSpace(raw)
		switch {
		case strings.HasPrefix(t, "# ") && !strings.HasPrefix(t, "## "):
			b.Name = strings.TrimSpace(t[2:])
		case strings.HasPrefix(t, "## "):
			name := strings.TrimSpace(t[3:])
			if name == "" {
				current = nil
				continue
			}
			// Strict mode (Skill / Memory) drops
			// sections that are not on the
			// fixed enum. Soul is free-form and
			// accepts any name.
			if strict && !isCanonicalSection(name) {
				current = nil
				continue
			}
			idx := findSection(b, name)
			if idx < 0 {
				b.Data = append(b.Data, Section{Name: name})
				idx = len(b.Data) - 1
			}
			current = &b.Data[idx]
		case t == "":
			// Blank line — keep current section
			// alive (text under the same section
			// is one logical entry).
		default:
			if current == nil {
				continue
			}
			val := strings.TrimPrefix(t, "- ")
			val = strings.TrimPrefix(val, "* ")
			// Numbered list: "1. text" or
			// "12. text". Drop the prefix
			// only when it is digits + dot.
			val = stripNumberedListPrefix(val)
			val = strings.TrimSpace(val)
			if val == "" {
				continue
			}
			if containsString(current.Values, val) {
				continue
			}
			current.Values = append(current.Values, val)
		}
	}
}

// findSection returns the index of the section with
// the given name, or -1.
func findSection(b *Base, name string) int {
	for i := range b.Data {
		if b.Data[i].Name == name {
			return i
		}
	}
	return -1
}

// isCanonicalSection reports whether the section
// name is on either fixed enum (Skill or Memory).
// Used by the strict migration path: a section
// not on the union is dropped.
func isCanonicalSection(name string) bool {
	for _, s := range SkillFixedSections {
		if s == name {
			return true
		}
	}
	for _, s := range MemoryFixedSections {
		if s == name {
			return true
		}
	}
	return false
}

// stripNumberedListPrefix drops a leading "N. "
// (digits + dot + space) from a list item. Used by
// the migration path; only fires when the prefix
// is purely numeric.
func stripNumberedListPrefix(s string) string {
	dot := strings.IndexByte(s, '.')
	if dot <= 0 {
		return s
	}
	prefix := s[:dot]
	for _, r := range prefix {
		if r < '0' || r > '9' {
			return s
		}
	}
	rest := s[dot+1:]
	rest = strings.TrimPrefix(rest, " ")
	return rest
}
