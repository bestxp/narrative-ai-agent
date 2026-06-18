package yaml

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/bestxp/narrative-ai-agent/internal/domain"
	"github.com/bestxp/narrative-ai-agent/internal/prompts"
	"github.com/bestxp/narrative-ai-agent/internal/storage"
)

// stateKey returns the canonical storage key for a
// world's state file.
func stateKey(world string) string {
	return "worlds/" + world + "/state.md"
}

// WorldStateYaml is the YAML-backed implementation of
// WorldStateRepository. The on-disk format is a
// hand-written markdown document rendered by the
// state.md.tmpl template — NOT a YAML file. The
// "YAML" name in this package refers to the project's
// convention (state.md is a project-specific format,
// not JSON or anything SQL would speak natively).
//
// SQL/noSQL backends would implement this as a single
// row per world with columns matching StateSnapshot.
// The interface stays the same.
type WorldStateYaml struct {
	store storage.Storage
}

// NewWorldStateYaml constructs the YAML-backed
// WorldStateRepository.
func NewWorldStateYaml(store storage.Storage) *WorldStateYaml {
	return &WorldStateYaml{store: store}
}

// Load reads state.md and parses it back into a
// StateSnapshot. An empty body returns the zero-value
// StateSnapshot (the world has no state yet).
func (r *WorldStateYaml) Load(world string) (domain.StateSnapshot, error) {
	body, err := r.store.Read(stateKey(world))
	if err != nil {
		return domain.StateSnapshot{}, err
	}
	return ParseStateMD(string(body)), nil
}

// Save renders the StateSnapshot through the
// state.md.tmpl template and writes it atomically.
func (r *WorldStateYaml) Save(world string, snap domain.StateSnapshot) error {
	body, err := renderStateBody(snap)
	if err != nil {
		return err
	}
	return r.store.Write(stateKey(world), []byte(body))
}

// AppendEvent is the read-modify-write helper for the
// day's chronology log. Loads the snapshot, appends
// the event (with whitespace-trimmed dedup), and saves
// it back. The atomicity comes from the storage
// backend (file: temp+rename; SQL: implicit
// transaction).
func (r *WorldStateYaml) AppendEvent(world, event string) error {
	snap, err := r.Load(world)
	if err != nil {
		return err
	}
	event = strings.TrimSpace(event)
	if event == "" {
		return nil
	}
	// Dedup: case-insensitive, whitespace-trimmed.
	// Existing events from the same day stay anchored;
	// re-emitting an identical bullet is a no-op.
	key := strings.ToLower(event)
	for _, existing := range snap.Events {
		if strings.ToLower(strings.TrimSpace(existing)) == key {
			return nil
		}
	}
	snap.Events = append(snap.Events, event)
	return r.Save(world, snap)
}

// EnsureExists writes a minimal placeholder if the
// world has no state file yet. Used by /launch.
func (r *WorldStateYaml) EnsureExists(world string, day int, inFlight bool) error {
	exists, err := r.store.Exists(stateKey(world))
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	return r.Save(world, domain.StateSnapshot{
		World:    world,
		Day:      day,
		InFlight: inFlight,
	})
}

// Compile-time guard.
// renderStateBody is the data-bag driven renderer for
// state.md. The template (prompts/state.md.tmpl) owns
// block order and conditional formatting; this helper
// only projects the in-memory StateSnapshot into the
// data-bag shape.
func renderStateBody(s domain.StateSnapshot) (string, error) {
	data := prompts.NewStateData(
		s.World, s.Day, s.InFlight,
		s.Location, s.Moment,
		s.NPCs, s.Events,
	)
	return prompts.Render("state.md.tmpl", prompts.PromptData{
		State: data,
	})
}

// RenderStateBody is the public alias of
// renderStateBody. Exposed so callers in other
// packages (e.g. the /me snapshot view) render a
// StateSnapshot back to the canonical markdown
// without duplicating the render logic.
func RenderStateBody(s domain.StateSnapshot) (string, error) {
	return renderStateBody(s)
}

// ParseStateMD is the inverse of renderStateBody —
// recovers the StateSnapshot from a state.md body.
// Tolerates a missing "## Хронология дня" section
// (returns empty Events). Tolerant of partial state
// files; missing fields stay zero.
//
// Block format:
//
//	# Состояние мира: <World>
//	День <N> (в процессе|завершён).
//	Локация: <Location>.
//	NPC: <npc1>, <npc2>, <npc3>.
//	Момент: <Moment>.
//	## Текущий момент
//	## Хронология дня
//	- <event 1>
//	- <event 2>
func ParseStateMD(body string) domain.StateSnapshot {
	out := domain.StateSnapshot{}
	if body == "" {
		return out
	}
	lines := strings.Split(body, "\n")
	inEvents := false
	for _, ln := range lines {
		trim := strings.TrimSpace(ln)
		switch {
		case strings.HasPrefix(trim, "# Состояние мира:"):
			out.World = strings.TrimSpace(strings.TrimPrefix(trim, "# Состояние мира:"))
		case strings.HasPrefix(trim, "## Текущий момент"):
			inEvents = false
		case strings.HasPrefix(trim, "## Хронология дня"):
			inEvents = true
		case inEvents && strings.HasPrefix(trim, "- "):
			out.Events = append(out.Events, strings.TrimSpace(strings.TrimPrefix(trim, "- ")))
		case strings.HasPrefix(trim, "День "):
			parts := strings.SplitN(trim, " ", 3)
			if len(parts) >= 2 {
				if d, err := strconv.Atoi(parts[1]); err == nil {
					out.Day = d
				}
			}
			out.InFlight = strings.Contains(trim, "в процессе")
		case strings.HasPrefix(trim, "Локация:"):
			out.Location = strings.TrimSpace(strings.TrimPrefix(trim, "Локация:"))
			out.Location = strings.TrimSuffix(out.Location, ".")
		case strings.HasPrefix(trim, "NPC:"):
			raw := strings.TrimSpace(strings.TrimPrefix(trim, "NPC:"))
			raw = strings.TrimSuffix(raw, ".")
			if raw != "" {
				for _, p := range strings.Split(raw, ",") {
					if n := strings.TrimSpace(p); n != "" {
						out.NPCs = append(out.NPCs, n)
					}
				}
			}
		case strings.HasPrefix(trim, "Момент:"):
			out.Moment = strings.TrimSpace(strings.TrimPrefix(trim, "Момент:"))
			out.Moment = strings.TrimSuffix(out.Moment, ".")
		}
	}
	return out
}

// ValidateStateFormat checks that a state.md body has
// the minimum fields required to be useful
// (World + Day > 0). Used by tests + the operator's
// /inspect command.
func ValidateStateFormat(body string) error {
	snap := ParseStateMD(body)
	if snap.World == "" {
		return fmt.Errorf("state.md: missing '# Состояние мира: <world>' header")
	}
	if snap.Day <= 0 {
		return fmt.Errorf("state.md: missing or invalid 'День N' line")
	}
	return nil
}

// (linter-quiet — context is reserved for future
// context-aware methods that take a deadline).
var _ = context.Background
