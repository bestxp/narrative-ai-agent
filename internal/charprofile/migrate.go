package charprofile

import (
	"errors"
	"fmt"
	"slices"
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
// # DATA-PRESERVATION CONTRACT
//
// MigrateFromMarkdown is LOSS-LESS. The strict
// section enum is enforced ONLY at write-time
// (Append, ReplaceSection). Migration must keep
// every `## <section>` heading the legacy file had,
// even if the name is not on the canonical enum —
// dropping it would silently delete the player's
// memory. The next Append call will then refile the
// legacy section's value into a canonical bucket
// when the model is ready, but until that happens
// the data is preserved.
//
// The character name (for SOUL.yaml) is taken from
// the legacy H1 if present, else from fileSlug. The
// "soul" field (Soul only) is left empty — the
// LLM-driven path is the right tool to fill it; a
// deterministic parser that guesses a soul line
// would just be wrong.
func MigrateFromMarkdown(kind string, body, fileSlug string) (any, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return nil, ErrNotFound
	}
	switch kind {
	case "SOUL":
		var s Soul
		// name from H1 ("# <Title>"); fall back
		// to fileSlug if the file has no H1
		// (extremely rare — every legacy .md had
		// a free-form title).
		if n := extractH1(body); n != "" {
			s.Name = n
		} else {
			s.Name = fileSlug
		}
		parseMarkdownSections(body, &s.Data, false)

		return s, nil
	case "skill":
		var s Skill
		parseMarkdownSections(body, &s.Data, true)

		return s, nil
	case "memory":
		var m Memory
		// LOSS-LESS: keep every `## <section>` even
		// if the name is not on MemoryFixedSections.
		// See the data-preservation contract above.
		parseMarkdownSections(body, &m.Data, false)

		return m, nil
	}

	return nil, fmt.Errorf("%w: %s", ErrUnknownFile, kind)
}

// parseMarkdownSections walks the body line-by-line
// and populates data. The strict flag controls
// whether unknown section names are dropped (Soul:
// never, Memory in legacy: never — see
// MigrateFromMarkdown contract; Skill: yes, only
// canonical names survive).
//
// The H1 line ("# Some Title") is NOT stored in
// data — MigrateFromMarkdown reads it via
// extractH1() when it needs the legacy title. The
// title is free-form prose (e.g. "Маркус — Ядро
// персонажа") and is not a section the model
// would ever call Append on.
func parseMarkdownSections(body string, data *[]Section, strict bool) {
	lines := strings.Split(body, "\n")
	var current *Section
	for _, raw := range lines {
		t := strings.TrimSpace(raw)
		switch {
		case strings.HasPrefix(t, "# ") && !strings.HasPrefix(t, "## "):
			// H1 ignored — b.Name is set by the caller
			// (MigrateFromMarkdown uses fileSlug, which
			// is the canonical character dir name and
			// matches the rest of the YAML tree).
			continue
		case strings.HasPrefix(t, "## "):
			name := strings.TrimSpace(t[3:])
			if name == "" {
				current = nil
				continue
			}
			// Strict mode (Skill migration only)
			// drops sections that are not on the
			// fixed enum. Soul and Memory migrations
			// are LOSS-LESS — see the
			// data-preservation contract in
			// MigrateFromMarkdown.
			if strict && !isCanonicalSection(name) {
				current = nil
				continue
			}
			idx := findSection(data, name)
			if idx < 0 {
				*data = append(*data, Section{Name: name})
				idx = len(*data) - 1
			}
			current = &(*data)[idx]
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
func findSection(data *[]Section, name string) int {
	for i := range *data {
		if (*data)[i].Name == name {
			return i
		}
	}

	return -1
}

// extractH1 returns the first H1 line's body
// ("# Foo" -> "Foo") or "" if there is no H1. Used
// by MigrateFromMarkdown to seed the Soul.Name from
// the legacy free-form title.
func extractH1(body string) string {
	for line := range strings.SplitSeq(body, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "# ") && !strings.HasPrefix(t, "## ") {
			return strings.TrimSpace(t[2:])
		}
	}

	return ""
}

// isCanonicalSection reports whether the section
// name is on either fixed enum (Skill or Memory).
// Used by the strict migration path: a section
// not on the union is dropped.
func isCanonicalSection(name string) bool {
	return slices.Contains(SkillFixedSections, name) || slices.Contains(MemoryFixedSections, name)
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
