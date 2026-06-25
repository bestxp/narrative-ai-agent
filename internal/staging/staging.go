// Package staging is the canonical on-disk shape and in-memory
// representation of a world's staged story graph.
//
// A world may be:
//   - sandbox  (staging.yaml enabled=false): staging is ignored.
//   - staged   (staging.yaml enabled=true): the world follows a graph
//     of story stages with conditional next transitions.
//
// The package provides Load/Save/UpdateStage/AdvanceTimeline/ApplyPending
// and a Render method that produces the markdown block injected into the
// WorldState user message.
package staging

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/bestxp/narrative-ai-agent/internal/limits"
	"gopkg.in/yaml.v3"
)

// MaxStageRenderBytes is the hard cap on the rendered size of the
// active stage block injected into the WorldState prompt. It protects
// against accidental prompt bloat.
//
// Re-exported from internal/limits so the staging
// renderer and the LLM-side templates share the same
// value. The local alias is kept for back-compat with
// every existing caller in this package.
const MaxStageRenderBytes = limits.StageRenderMaxBytes

// FileStore is the minimal storage surface the staging package needs.
type FileStore interface {
	ReadRaw(rel string) (string, error)
	WriteRawAtomic(rel, body string) error
	Exists(rel string) bool
}

// Staging is the loaded state of staging.yaml + stage.md for one world.
type Staging struct {
	Enabled       bool
	Current       Stage
	TimelineIndex int
	Next          string
	// raw is kept for debugging/diagnostics only.
	raw stagingFile
}

// Stage is one node in the story graph.
type Stage struct {
	ID          string
	Name        string
	Description string
	Timeline    []TimelinePoint
	Next        []Transition
}

// TimelinePoint is one beat inside a stage.
type TimelinePoint struct {
	Days        string
	Info        string
	Description string
}

// Transition is a possible exit from a stage.
type Transition struct {
	ID           string
	Requirements []string
}

// stageFile is the on-disk shape of staging.yaml.
type stagingFile struct {
	Enabled bool        `yaml:"enabled"`
	Init    []string    `yaml:"init"`
	Stages  []stageYAML `yaml:"stages"`
}

type stageYAML struct {
	ID          string           `yaml:"id"`
	Name        string           `yaml:"name"`
	Description string           `yaml:"description"`
	Timeline    []timelineYAML   `yaml:"timeline"`
	Next        []transitionYAML `yaml:"next"`
}

type timelineYAML struct {
	Days        string `yaml:"days"`
	Info        string `yaml:"info"`
	Description string `yaml:"description"`
}

type transitionYAML struct {
	ID           string   `yaml:"id"`
	Requirements []string `yaml:"requirements"`
}

// stageStateFile used to be the on-disk shape of stage.md —
// merged into state.yaml in planning/0001. The
// runtime slice of the graph now lives in
// domain.StageState and is written/read by
// repository/yaml/world_state_yaml.go alongside
// the rest of the runtime snapshot.

// Load reads staging.yaml for world from fs. If
// staging.yaml is missing or enabled=false, it returns
// a zero Staging with Enabled=false and no error.
//
// planning/0001: the runtime stage slice (current /
// timeline_index / next) is no longer in stage.md —
// it's part of state.yaml. Load takes an explicit
// runtimeState argument (parsed from state.yaml by
// the caller) so the in-memory Staging mirrors
// exactly what was last persisted. A missing or
// empty runtimeState means "no graph resolved yet" —
// Load defaults Current to init[0] and TimelineIndex
// to 0 (the canonical start of any staged world).
func Load(fs FileStore, world string, runtimeState StageRuntime) (*Staging, error) {
	stagingPath := stagingPath(world)
	stagingBody, err := fs.ReadRaw(stagingPath)
	if err != nil {
		return nil, fmt.Errorf("staging: read %s: %w", stagingPath, err)
	}
	if strings.TrimSpace(stagingBody) == "" {
		return &Staging{Enabled: false}, nil
	}

	var raw stagingFile
	if err := yaml.Unmarshal([]byte(stagingBody), &raw); err != nil {
		return nil, fmt.Errorf("staging: parse %s: %w", stagingPath, err)
	}

	if !raw.Enabled {
		return &Staging{Enabled: false}, nil
	}

	if err := validateStagingFile(&raw); err != nil {
		return nil, fmt.Errorf("staging: validate %s: %w", stagingPath, err)
	}

	currentID := runtimeState.Current
	if currentID == "" {
		if len(raw.Init) == 0 {
			return nil, errors.New("staging: enabled but init is empty")
		}
		currentID = raw.Init[0]
	}

	stageMap := buildStageMap(&raw)

	current, ok := stageMap[currentID]
	if !ok {
		return nil, fmt.Errorf("staging: current stage %q not found in %s", currentID, stagingPath)
	}

	// Repair broken next.
	next := repairNext(runtimeState.Next, stageMap, current)

	// Repair broken timeline index.
	idx := runtimeState.TimelineIndex
	if idx < 0 || idx >= len(current.Timeline) {
		idx = 0
	}

	return &Staging{
		Enabled:       true,
		Current:       current,
		TimelineIndex: idx,
		Next:          next,
		raw:           raw,
	}, nil
}

// StageRuntime is the runtime slice of the plot
// graph, taken from state.yaml (planning/0001:
// state.yaml.stage). The staging package only reads
// these three primitives — full graph data lives in
// staging.yaml and is loaded separately.
type StageRuntime struct {
	Current       string
	TimelineIndex int
	Next          string
}

// Runtime projects the in-memory staging state back
// into the three primitives that belong in
// state.yaml.stage. Used by callers that write to
// state.yaml (gm.go end-of-day, UpdateState, etc.).
// Pure function — no disk I/O, no Save call. The
// single writer to state.yaml is
// repository/yaml.world_state_yaml.
func (s *Staging) Runtime() StageRuntime {
	if s == nil || !s.Enabled {
		return StageRuntime{}
	}

	return StageRuntime{
		Current:       s.Current.ID,
		TimelineIndex: s.TimelineIndex,
		Next:          s.Next,
	}
}

// UpdateStage sets Next to nextID. It validates that
// nextID is a known transition from the current
// stage. In-memory only — the caller must write the
// resulting state to state.yaml via
// repository/yaml.world_state_yaml (planning/0001:
// no more stage.md).
func (s *Staging) UpdateStage(_ FileStore, _, nextID string) error {
	if nextID == "" {
		return errors.New("staging: next_id is empty")
	}
	found := false
	for _, t := range s.Current.Next {
		if t.ID == nextID {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("staging: %q is not a valid transition from %q", nextID, s.Current.ID)
	}

	s.Next = nextID

	return nil
}

// AdvanceTimeline increments TimelineIndex if
// possible. In-memory only — same constraint as
// UpdateStage.
func (s *Staging) AdvanceTimeline(_ FileStore, _ string) error {
	if s.TimelineIndex+1 >= len(s.Current.Timeline) {
		return errors.New("staging: already at last timeline point")
	}

	s.TimelineIndex++

	return nil
}

// ApplyPending moves Next into Current and resets
// TimelineIndex. It is a no-op if Next is empty.
// In-memory only — same constraint as UpdateStage.
// The returned StageRuntime reflects the post-apply
// state so the caller can persist it (planning/0001:
// stage.md was merged into state.yaml; the single
// writer of the runtime slice is WorldState.Save).
func (s *Staging) ApplyPending(_ FileStore, _ string) (StageRuntime, error) {
	if s.Next == "" {
		return StageRuntime{
			Current:       s.Current.ID,
			TimelineIndex: s.TimelineIndex,
			Next:          s.Next,
		}, nil
	}
	stageMap := buildStageMap(&s.raw)
	next, ok := stageMap[s.Next]
	if !ok {
		return StageRuntime{}, fmt.Errorf("staging: pending stage %q not found", s.Next)
	}
	s.Current = next
	s.TimelineIndex = 0
	s.Next = ""

	return StageRuntime{
		Current:       s.Current.ID,
		TimelineIndex: s.TimelineIndex,
		Next:          s.Next,
	}, nil
}

// Render returns the markdown block for the WorldState prompt.
func (s *Staging) Render(characterName string) string {
	if !s.Enabled {
		return ""
	}

	var b strings.Builder
	b.WriteString("### Сюжетная стадия\n")
	fmt.Fprintf(&b, "**%s — %s**\n", s.Current.ID, s.Current.Name)
	if s.Next != "" {
		fmt.Fprintf(&b, "(завершается) → %s\n", s.Next)
	}
	b.WriteString("\n")

	desc := s.Current.Description
	if characterName != "" {
		desc = strings.ReplaceAll(desc, "$(name)", characterName)
	}
	b.WriteString(strings.TrimSpace(desc))
	b.WriteString("\n\n")

	if len(s.Current.Timeline) > 0 {
		b.WriteString("**Таймлайн:**\n")
		for i, p := range s.Current.Timeline {
			prefix := "[ ]"
			if i < s.TimelineIndex {
				prefix = "[X]"
			} else if i == s.TimelineIndex {
				prefix = "[>]"
			}
			fmt.Fprintf(&b, "%s %s: %s\n", prefix, p.Days, p.Info)
		}
		b.WriteString("\n")
	}

	if len(s.Current.Next) > 0 {
		b.WriteString("**Возможные переходы:**\n")
		for _, t := range s.Current.Next {
			reqs := strings.Join(t.Requirements, "; ")
			fmt.Fprintf(&b, "- → %s, если: %s\n", t.ID, reqs)
		}
	}

	out := strings.TrimSpace(b.String())
	if len(out) > MaxStageRenderBytes {
		out = out[:MaxStageRenderBytes] + "\n..."
	}
	return out
}

// --- helpers ---

func stagingPath(world string) string {
	return "worlds/" + world + "/staging.yaml"
}

// repairNext clears next when it points at an unknown
// stage or a transition that is not listed in
// current.Next. Split out from Load to keep the
// nestif complexity under threshold (the inlined
// version nests three levels).
func repairNext(next string, stageMap map[string]Stage, current Stage) string {
	if next == "" {
		return ""
	}

	if _, ok := stageMap[next]; !ok {
		return ""
	}

	for _, t := range current.Next {
		if t.ID == next {
			return next
		}
	}

	return ""
}

func buildStageMap(raw *stagingFile) map[string]Stage {
	out := make(map[string]Stage, len(raw.Stages))
	for _, sy := range raw.Stages {
		timeline := make([]TimelinePoint, len(sy.Timeline))
		for i, ty := range sy.Timeline {
			timeline[i] = TimelinePoint{
				Days:        normalizeDays(ty.Days),
				Info:        strings.TrimSpace(ty.Info),
				Description: strings.TrimSpace(ty.Description),
			}
		}
		next := make([]Transition, len(sy.Next))
		for i, ny := range sy.Next {
			reqs := make([]string, len(ny.Requirements))
			for j, r := range ny.Requirements {
				reqs[j] = strings.TrimSpace(r)
			}
			next[i] = Transition{
				ID:           strings.TrimSpace(ny.ID),
				Requirements: reqs,
			}
		}
		out[strings.TrimSpace(sy.ID)] = Stage{
			ID:          strings.TrimSpace(sy.ID),
			Name:        strings.TrimSpace(sy.Name),
			Description: strings.TrimSpace(sy.Description),
			Timeline:    timeline,
			Next:        next,
		}
	}
	return out
}

func validateStagingFile(raw *stagingFile) error {
	if !raw.Enabled {
		return nil
	}
	if len(raw.Init) == 0 {
		return errors.New("enabled=true but init is empty")
	}

	ids := make(map[string]struct{})
	for _, st := range raw.Stages {
		id := strings.TrimSpace(st.ID)
		if id == "" {
			return errors.New("stage with empty id")
		}
		if _, ok := ids[id]; ok {
			return fmt.Errorf("duplicate stage id %q", id)
		}
		ids[id] = struct{}{}
	}

	hasDays := false
	noDays := false
	for _, st := range raw.Stages {
		if strings.TrimSpace(st.Name) == "" {
			return fmt.Errorf("stage %q: empty name", st.ID)
		}
		for _, p := range st.Timeline {
			if strings.TrimSpace(p.Info) == "" {
				return fmt.Errorf("stage %q: timeline point with empty info", st.ID)
			}
			if strings.TrimSpace(p.Description) == "" {
				return fmt.Errorf("stage %q: timeline point %q: empty description", st.ID, p.Info)
			}
			if p.Days != "" {
				hasDays = true
				if _, _, err := parseDaysRange(p.Days); err != nil {
					return fmt.Errorf("stage %q: timeline point %q: invalid days %q: %w", st.ID, p.Info, p.Days, err)
				}
			} else {
				noDays = true
			}
		}
		if len(st.Next) == 0 {
			return fmt.Errorf("stage %q: no transitions", st.ID)
		}
		for _, n := range st.Next {
			if strings.TrimSpace(n.ID) == "" {
				return fmt.Errorf("stage %q: transition with empty id", st.ID)
			}
			if _, ok := ids[n.ID]; !ok {
				return fmt.Errorf("stage %q: transition to unknown stage %q", st.ID, n.ID)
			}
			if len(n.Requirements) == 0 {
				return fmt.Errorf("stage %q: transition %q: no requirements", st.ID, n.ID)
			}
		}
	}
	if hasDays && noDays {
		return errors.New("timeline days must be either present for all points or absent for all")
	}

	for _, init := range raw.Init {
		if _, ok := ids[init]; !ok {
			return fmt.Errorf("init stage %q not found", init)
		}
	}

	return nil
}

func normalizeDays(v interface{}) string {
	switch v := v.(type) {
	case string:
		return strings.TrimSpace(v)
	case int:
		return strconv.Itoa(v)
	default:
		return fmt.Sprintf("%v", v)
	}
}

func parseDaysRange(s string) (min, max int, err error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, 0, nil
	}
	if strings.Contains(s, "-") {
		parts := strings.SplitN(s, "-", 2)
		min, err = strconv.Atoi(strings.TrimSpace(parts[0]))
		if err != nil {
			return 0, 0, err
		}
		max, err = strconv.Atoi(strings.TrimSpace(parts[1]))
		if err != nil {
			return 0, 0, err
		}
		if min > max {
			return 0, 0, fmt.Errorf("min > max")
		}
		return min, max, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, 0, err
	}
	return n, n, nil
}
