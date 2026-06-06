package domain

import (
	"fmt"
	"strings"
)

// BuildStateMarkdown renders the worlds/<w>/state.md file body
// from a structured StateSnapshot. The format is intentionally
// parseable by the LLM (it sees the same file via the system
// prompt) and append-friendly for the "Хронология дня" section
// which the bot grows as the player makes turns.
//
// The schema:
//
//	# Состояние мира: <world>
//
//	## Текущий момент
//	День 5 (в процессе).
//	Локация: Коноха, допрос.
//	NPC: anbu_dog, anbu_cat.
//	Момент: Аньбу толкает Маркуса в спину.
//
//	## Хронология дня
//	- Ход 12: Аньбу остановила Маркуса.
//	- Ход 13: Маркус назвался.
//
// History section is left out — it lives in system_state.md
// (see domain.SystemState). Keeping state.md strictly narrative
// means the static system prompt prefix (the part the LLM
// provider can cache) stays stable across compactions; only
// the Хронология section grows and that is also appended, not
// rewritten.
func BuildStateMarkdown(s StateSnapshot) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Состояние мира: %s\n\n", s.World)

	b.WriteString("## Текущий момент\n")
	if s.InFlight {
		fmt.Fprintf(&b, "День %d (в процессе).\n", s.Day)
	} else {
		fmt.Fprintf(&b, "День %d (завершён).\n", s.Day)
	}
	if s.Location != "" {
		fmt.Fprintf(&b, "Локация: %s.\n", s.Location)
	}
	if len(s.NPCs) > 0 {
		fmt.Fprintf(&b, "NPC: %s.\n", strings.Join(s.NPCs, ", "))
	}
	if s.Moment != "" {
		fmt.Fprintf(&b, "Момент: %s.\n", s.Moment)
	}
	b.WriteString("\n")

	if len(s.Events) > 0 {
		b.WriteString("## Хронология дня\n")
		for _, e := range s.Events {
			fmt.Fprintf(&b, "- %s\n", e)
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// StateSnapshot is the in-memory representation of a state's
// contents. The usecase layer is responsible for loading and
// persisting this — domain just shapes the format.
type StateSnapshot struct {
	World    string
	Day      int
	InFlight bool
	Location string
	NPCs     []string
	Moment   string
	Events   []string
}
