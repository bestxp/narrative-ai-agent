package usecase

import (
	"regexp"
	"strings"
)

// CompactNPCBody performs a semantic condensation of an NPC file. It
// keeps the "what the NPC became" and "how they see the player" —
// dialogue logs and chronological repetition are removed. The function
// is intentionally conservative: anything that looks like an event
// entry is stripped, but anchors (headers, bullets, character facts)
// are preserved.
func CompactNPCBody(body string) string {
	lines := strings.Split(body, "\n")
	keep := make([]string, 0, len(lines)/2)
	eventRe := regexp.MustCompile(`^\s*(?:-|\*|\d+\.)\s+.*\d{4}.*`) // dated bullets
	quoteRe := regexp.MustCompile(`^\s*>.+`)                        // blockquotes

	for _, ln := range lines {
		trimmed := strings.TrimSpace(ln)
		if trimmed == "" {
			keep = append(keep, ln)

			continue
		}

		if strings.HasPrefix(trimmed, "#") {
			keep = append(keep, ln)

			continue
		}

		if quoteRe.MatchString(ln) {
			continue
		}

		if eventRe.MatchString(ln) {
			continue
		}

		keep = append(keep, ln)
	}

	out := strings.Join(keep, "\n")

	return strings.TrimSpace(out) + "\n"
}
