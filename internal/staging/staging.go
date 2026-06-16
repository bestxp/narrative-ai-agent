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

	"gopkg.in/yaml.v3"

	"github.com/bestxp/narrative-ai-agent/internal/limits"
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

// stageStateFile is the on-disk shape of stage.md.
type stageStateFile struct {
	Staging struct {
		Current       string `yaml:"current"`
		TimelineIndex int    `yaml:"timeline_index"`
		Next          string `yaml:"next"`
	} `yaml:"staging"`
}

// Load reads staging.yaml and stage.md for world from fs.
// If staging.yaml is missing or enabled=false, it returns a zero Staging
// with Enabled=false and no error.
func Load(fs FileStore, world string) (*Staging, error) {
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

	statePath := stageStatePath(world)
	stateBody, _ := fs.ReadRaw(statePath)
	state, err := parseStageState(stateBody)
	if err != nil {
		return nil, fmt.Errorf("staging: parse %s: %w", statePath, err)
	}

	// Default to init[0] if no state file.
	if state.Staging.Current == "" {
		if len(raw.Init) == 0 {
			return nil, errors.New("staging: enabled but init is empty")
		}
		state.Staging.Current = raw.Init[0]
		state.Staging.TimelineIndex = 0
		state.Staging.Next = ""
	}

	stageMap := buildStageMap(&raw)
	current, ok := stageMap[state.Staging.Current]
	if !ok {
		return nil, fmt.Errorf("staging: current stage %q not found in %s", state.Staging.Current, stagingPath)
	}

	// Repair broken next.
	if state.Staging.Next != "" {
		if _, ok := stageMap[state.Staging.Next]; !ok {
			state.Staging.Next = ""
		} else {
			// Also verify next is listed in current.Next.
			found := false
			for _, t := range current.Next {
				if t.ID == state.Staging.Next {
					found = true
					break
				}
			}
			if !found {
				state.Staging.Next = ""
			}
		}
	}

	// Repair broken timeline index.
	if state.Staging.TimelineIndex < 0 || state.Staging.TimelineIndex >= len(current.Timeline) {
		state.Staging.TimelineIndex = 0
	}

	return &Staging{
		Enabled:       true,
		Current:       current,
		TimelineIndex: state.Staging.TimelineIndex,
		Next:          state.Staging.Next,
		raw:           raw,
	}, nil
}

// Save persists the current staging state to stage.md.
func (s *Staging) Save(fs FileStore, world string) error {
	state := stageStateFile{}
	state.Staging.Current = s.Current.ID
	state.Staging.TimelineIndex = s.TimelineIndex
	state.Staging.Next = s.Next

	out, err := yaml.Marshal(&state)
	if err != nil {
		return fmt.Errorf("staging: marshal stage state: %w", err)
	}
	return fs.WriteRawAtomic(stageStatePath(world), string(out))
}

// UpdateStage sets Next to nextID. It validates that nextID is a known
// transition from the current stage.
func (s *Staging) UpdateStage(fs FileStore, world, nextID string) error {
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
	return s.Save(fs, world)
}

// AdvanceTimeline increments TimelineIndex if possible.
func (s *Staging) AdvanceTimeline(fs FileStore, world string) error {
	if s.TimelineIndex+1 >= len(s.Current.Timeline) {
		return errors.New("staging: already at last timeline point")
	}
	s.TimelineIndex++
	return s.Save(fs, world)
}

// ApplyPending moves Next into Current and resets TimelineIndex.
// It is a no-op if Next is empty.
func (s *Staging) ApplyPending(fs FileStore, world string) error {
	if s.Next == "" {
		return nil
	}
	stageMap := buildStageMap(&s.raw)
	next, ok := stageMap[s.Next]
	if !ok {
		return fmt.Errorf("staging: pending stage %q not found", s.Next)
	}
	s.Current = next
	s.TimelineIndex = 0
	s.Next = ""
	return s.Save(fs, world)
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

func stageStatePath(world string) string {
	return "worlds/" + world + "/stage.md"
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

func parseStageState(body string) (stageStateFile, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return stageStateFile{}, nil
	}
	var s stageStateFile
	if err := yaml.Unmarshal([]byte(body), &s); err != nil {
		return stageStateFile{}, err
	}
	return s, nil
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
