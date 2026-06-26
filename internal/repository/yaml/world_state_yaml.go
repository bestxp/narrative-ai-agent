package yaml

import (
	"context"
	"fmt"
	"strings"

	"github.com/bestxp/narrative-ai-agent/internal/domain"
	"github.com/bestxp/narrative-ai-agent/internal/storage"
	gyaml "gopkg.in/yaml.v3"
)

// stateKey returns the canonical storage key for a
// world's state file. planning/0001 (state+stage
// merge): the file is YAML, not markdown. See
// running/game-data/worlds/naruto/state.yaml for the
// reference format.
func stateKey(world string) string {
	return "worlds/" + world + "/state.yaml"
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

// Load reads state.yaml and parses it back into a
// StateSnapshot. An empty body returns the zero-value
// StateSnapshot (the world has no state yet).
func (r *WorldStateYaml) Load(world string) (domain.StateSnapshot, error) {
	body, err := r.store.Read(stateKey(world))
	if err != nil {
		return domain.StateSnapshot{}, fmt.Errorf("world_state_load: Read failed: %w", err)
	}

	return parseStateYAML(string(body)), nil
}

// Save renders the StateSnapshot through the
// state.md.tmpl template and writes it atomically.
func (r *WorldStateYaml) Save(world string, snap domain.StateSnapshot) error {
	body, err := renderStateBody(snap)
	if err != nil {
		return err
	}

	if err := r.store.Write(stateKey(world), []byte(body)); err != nil {
		return fmt.Errorf("world_state.Save: write: %w", err)
	}

	return nil
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
		return fmt.Errorf("ensure_exists: Exists failed: %w", err)
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

// stateYAML is the wire shape of state.yaml. planning/0001
// (state.md + stage.md → state.yaml): two top-level blocks,
// `state:` (runtime snapshot) and `stage:` (active plot
// stage), both at the root of the document. The `stage:`
// block is a permanent part of state.yaml from the very
// first turn of a new world (a freshly-initialised world
// writes `{current: "", timeline_index: 0, next: ""}` —
// the "no graph yet" baseline). Marshal/Unmarshal is the
// only allowed writer path — no text template, no string
// concatenation.
type stateYAML struct {
	State stateYAMLState `yaml:"state"`
	Stage stateYAMLStage `yaml:"stage"`
}

type stateYAMLState struct {
	World    string   `yaml:"world"`
	Day      int      `yaml:"day"`
	InFlight bool     `yaml:"in_flight"`
	Daytime  string   `yaml:"daytime,omitempty"`
	Location string   `yaml:"location,omitempty"`
	Moment   string   `yaml:"moment,omitempty"`
	NPCs     []string `yaml:"npcs,omitempty"`
	Current  string   `yaml:"current,omitempty"`
	Events   []string `yaml:"events"`
}

type stateYAMLStage struct {
	Current       string `yaml:"current"`
	TimelineIndex int    `yaml:"timeline_index"`
	Next          string `yaml:"next"`
}

// renderStateBody serializes a StateSnapshot back to
// the canonical state.yaml bytes. planning/0001: there
// is no template — yaml.v3 owns field order and
// formatting. The `omitempty` tags on stateYAMLState
// keep the on-disk file minimal when fields are unset
// (matching the naruto.yaml example: optional daytime
// / moment / location / current / npcs collapse to
// absent rather than empty). The `stage:` block is
// always written — it's a permanent part of state.yaml.
func renderStateBody(s domain.StateSnapshot) (string, error) {
	doc := stateYAML{
		State: stateYAMLState{
			World:    s.World,
			Day:      s.Day,
			InFlight: s.InFlight,
			Daytime:  s.Daytime,
			Location: s.Location,
			Moment:   s.Moment,
			NPCs:     append([]string(nil), s.NPCs...),
			Current:  s.Current,
			Events:   append([]string(nil), s.Events...),
		},
		Stage: stateYAMLStage{
			Current:       s.Stage.Current,
			TimelineIndex: s.Stage.TimelineIndex,
			Next:          s.Stage.Next,
		},
	}

	out, err := gyaml.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("state: marshal: %w", err)
	}

	return string(out), nil
}

// RenderStateBody is the public alias of
// renderStateBody. Exposed so callers in other
// packages (e.g. the /me snapshot view) render a
// StateSnapshot back to the canonical state.yaml
// without duplicating the render logic.
func RenderStateBody(s domain.StateSnapshot) (string, error) {
	return renderStateBody(s)
}

// parseStateYAML is the inverse of renderStateBody —
// recovers the StateSnapshot from a state.yaml body.
// Tolerant of partial state files; missing fields
// stay zero.
//
// Block format (planning/0001, see
// running/game-data/worlds/naruto/state.yaml):
//
//	state:
//	  world: <world>
//	  day: <N>
//	  in-flight: true|false
//	  daytime: утро|день|вечер|ночь
//	  npcs:
//	    - <npc1>
//	  current: |-
//	    <one-line snapshot>
//	  events:
//	    - "<event 1>"
//	stage:
//	  current: <id>
//	  timeline_index: <N>
//	  next: <id|"">
func parseStateYAML(body string) domain.StateSnapshot {
	out := domain.StateSnapshot{}
	if strings.TrimSpace(body) == "" {
		return out
	}

	var doc stateYAML
	if err := gyaml.Unmarshal([]byte(body), &doc); err != nil {
		return out
	}

	out.World = doc.State.World
	out.Day = doc.State.Day
	out.InFlight = doc.State.InFlight
	out.Daytime = doc.State.Daytime
	out.NPCs = append(out.NPCs, doc.State.NPCs...)
	out.Current = doc.State.Current
	out.Location = doc.State.Location
	out.Moment = doc.State.Moment
	out.Events = append(out.Events, doc.State.Events...)
	out.Stage = domain.StageState{
		Current:       doc.Stage.Current,
		TimelineIndex: doc.Stage.TimelineIndex,
		Next:          doc.Stage.Next,
	}

	return out
}

// (linter-quiet — context is reserved for future
// context-aware methods that take a deadline).
var _ = context.Background
