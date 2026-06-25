package files

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"
	gyaml "gopkg.in/yaml.v3"

	"github.com/bestxp/narrative-ai-agent/internal/domain"
	"github.com/bestxp/narrative-ai-agent/internal/repository/api"
	"github.com/bestxp/narrative-ai-agent/internal/repository/yaml"
	"github.com/bestxp/narrative-ai-agent/internal/slowlog"
	"github.com/bestxp/narrative-ai-agent/internal/usecase/tools"
)

// renderStateBody delegates to the yaml package's
// canonical renderer (state.md.tmpl).
func renderStateBody(s domain.StateSnapshot) (string, error) {
	body, err := yaml.RenderStateBody(s)
	if err != nil {
		return "", fmt.Errorf("render_state_body: %w", err)
	}
	return body, nil
}

// State is the repository-backed implementation of
// tools.StateTool: state.md, plan.md and chronicle.yaml
// lifecycle. All persistent reads and writes go through
// the *api.Repositories bundle — there is no fs field
// and no domain code touches the storage layer
// directly.
type State struct {
	repos *api.Repositories
	log   zerolog.Logger
	// slow is the audit log; nil-safe (Write checks nil).
	slow *slowlog.Logger
	// chronicleCompress is invoked after a successful
	// chronicle append. Wired by NewFileToolset so that
	// the state writer does not need a direct dependency
	// on the memory struct. The hook is nil-safe and
	// no-ops when no summarizer is wired.
	chronicleCompress func(ctx context.Context, world string, dayJustArchived int) error
	// worldStateInvalidate is invoked after the day is
	// closed (ArchiveChronicleDay, end_day). Wired by
	// NewFileToolset to call GM.InvalidateWorldState so
	// the next turn rebuilds index:1 from disk. It is
	// also invoked by /reload (dispatcher) and
	// leave_world (world.go).
	worldStateInvalidate func(reason string)
}

func newState(log zerolog.Logger, slow *slowlog.Logger, repos *api.Repositories) *State {
	return &State{
		repos: repos,
		log:   log.With().Str("component", "state").Logger(),
		slow:  slow,
	}
}

// SetChronicleCompress wires the
// post-ArchiveChronicleDay hook. Called once at
// construction time from NewFileToolset.
func (s *State) SetChronicleCompress(fn func(ctx context.Context, world string, dayJustArchived int) error) {
	s.chronicleCompress = fn
}

// SetWorldStateInvalidate wires the post-day-close hook.
// Called once at construction time from NewFileToolset.
// Nil is fine — the dispatcher /reload path will call it
// directly via GM.InvalidateWorldState.
func (s *State) SetWorldStateInvalidate(fn func(reason string)) {
	s.worldStateInvalidate = fn
}

// NPCCompactLineThreshold is exported so the dispatcher's
// /maintenance command can match the same threshold the
// toolset uses.
const NPCCompactLineThreshold = 40

// UpdateState writes a trimmed "here and now" snapshot.
// The хронология дня section grows by appending
// AppendEvents — one entry per call. StateSnapshot.AppendEvents
// is a delta, not a replacement.
//
// All I/O goes through repos.WorldState (Load + Save).
func (s *State) UpdateState(snap tools.StateSnapshot) error {
	if snap.World == "" {
		// Resolve active world from the registry when the
		// caller didn't specify one.
		if info, err := s.repos.Info.Load(); err == nil {
			snap.World = info.ActiveWorld
		}
		if snap.World == "" {
			return errors.New("update_state: no active world")
		}
	}
	existing, err := s.repos.WorldState.Load(snap.World)
	if err != nil {
		return fmt.Errorf("update_state: WorldState.Load failed: %w", err)
	}
	// Snapshot the BEFORE picture for the slowlog diff.
	prevNPCs := append([]string(nil), existing.NPCs...)
	prevEventCount := len(existing.Events)
	existing.World = snap.World
	existing.Day = snap.Day
	existing.InFlight = snap.InFlight
	existing.Location = snap.Location
	existing.Moment = snap.Moment
	if snap.NPCs != nil {
		existing.NPCs = snap.NPCs
	}
	if len(snap.AppendEvents) > 0 {
		// Dedupe by case-insensitive, whitespace-trimmed
		// key. Without this, the chronology grows by N
		// events per turn and repeats the same line N
		// times.
		seen := make(map[string]struct{}, len(existing.Events))
		for _, e := range existing.Events {
			seen[normaliseEventKey(e)] = struct{}{}
		}
		for _, e := range snap.AppendEvents {
			key := normaliseEventKey(e)
			if key == "" {
				continue
			}
			if _, dup := seen[key]; dup {
				s.log.Debug().Str("event", e).Msg("update_state: duplicate event dropped")
				continue
			}
			seen[key] = struct{}{}
			existing.Events = append(existing.Events, e)
		}
	}
	body, err := renderStateBody(existing)
	if err != nil {
		return err
	}
	s.log.Info().
		Int("day", snap.Day).
		Bool("in_flight", snap.InFlight).
		Int("npcs", len(snap.NPCs)).
		Int("events", len(existing.Events)).
		Msg("update_state")
	if err := s.repos.WorldState.Save(snap.World, existing); err != nil {
		return fmt.Errorf("update_state: WorldState.Save failed: %w", err)
	}
	if s.slow != nil {
		_ = s.slow.Write("tool.update_state", "", map[string]any{
			"day":          snap.Day,
			"in_flight":    snap.InFlight,
			"moment":       snap.Moment,
			"npcs_added":   diffStrings(existing.NPCs, prevNPCs),
			"npcs_removed": diffStrings(prevNPCs, existing.NPCs),
			"npcs_now":     existing.NPCs,
			"events_added": len(existing.Events) - prevEventCount,
			"path":         "worlds/" + snap.World + "/state.yaml",
			"bytes":        len(body),
		})
	}
	return nil
}

// diffStrings returns elements present in `a` but not in `b`,
// preserving order.
func diffStrings(a, b []string) []string {
	bs := make(map[string]struct{}, len(b))
	for _, s := range b {
		bs[strings.TrimSpace(s)] = struct{}{}
	}
	var out []string
	for _, s := range a {
		if _, ok := bs[strings.TrimSpace(s)]; !ok {
			out = append(out, strings.TrimSpace(s))
		}
	}
	if out == nil {
		return []string{}
	}
	return out
}

// ParseStateYAML decodes the day counter from a
// state.yaml body. planning/0001 (state+stage
// merge): the on-disk format is YAML, not the legacy
// markdown. Returns 0 when the body is empty or
// unparseable — callers treat 0 as "unknown".
//
// Why this is local to the tools/files package: the
// full StateSnapshot parse lives in
// internal/repository/yaml/world_state_yaml.go
// (ParseStateYAML), which is the canonical round-trip
// for the writer tool path. The state-tool path
// (UpdateState / ArchiveChronicleDay) does not need
// the full snapshot — just the day counter — so the
// narrow helper stays here.
func ParseStateYAML(body string) int {
	if strings.TrimSpace(body) == "" {
		return 0
	}

	var raw struct {
		State struct {
			Day int `yaml:"day"`
		} `yaml:"state"`
	}
	if err := gyaml.Unmarshal([]byte(body), &raw); err != nil {
		return 0
	}

	return raw.State.Day
}

// ParseStateYAMLFull decodes the full StateSnapshot
// from a state.yaml body. planning/0001 (state+stage
// merge): the on-disk format is YAML, not markdown.
// See running/game-data/worlds/naruto/state.yaml for
// the reference shape. Tolerant of partial state
// files; missing fields stay zero.
//
// This is a thin convenience wrapper that callers
// without an active repos.WorldState (e.g. scene-key
// derivation in gm.buildContext, which only has the
// fs.FileStore at hand) use to peek at the day
// counter without spinning up a full repository
// stack. For everything else, prefer
// `s.repos.WorldState.Load(world)`.
func ParseStateYAMLFull(body string) domain.StateSnapshot {
	out := domain.StateSnapshot{}
	if strings.TrimSpace(body) == "" {
		return out
	}

	var doc struct {
		State struct {
			World    string   `yaml:"world"`
			Day      int      `yaml:"day"`
			InFlight bool     `yaml:"in-flight"`
			Daytime  string   `yaml:"daytime"`
			NPCs     []string `yaml:"npcs"` //nolint:tagliatelle // npcs is an acronym; kebab split harms readability
			Current  string   `yaml:"current"`
			Location string   `yaml:"location"`
			Moment   string   `yaml:"moment"`
			Events   []string `yaml:"events"`
		} `yaml:"state"`
		Stage struct {
			Current       string `yaml:"current"`
			TimelineIndex int    `yaml:"timeline-index"`
			Next          string `yaml:"next"`
		} `yaml:"stage"`
	}
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

// RotatePlan replaces plan.md content via repos.Plan.
func (s *State) RotatePlan(world string, events []string) error {
	if err := s.repos.Plan.ReplaceEvents(context.Background(), world, events); err != nil {
		return fmt.Errorf("rotate_plan: ReplaceEvents failed: %w", err)
	}
	return nil
}

// PlanRangeError signals that the caller passed a
// non-canonical number of upcoming events.
type PlanRangeError struct{ Given int }

func (e *PlanRangeError) Error() string {
	return "plan.md must contain 3-5 events, got " + strconv.Itoa(e.Given)
}

// ArchiveChronicleDay appends a new day entry to the
// world's chronicle via repos.Chronicle, then triggers
// the chronicle-compression hook whenever a window
// closes.
func (s *State) ArchiveChronicleDay(ctx context.Context, world string, day int, summary string) error {
	if strings.TrimSpace(summary) == "" {
		s.log.Debug().Int("day", day).Msg("archive_chronicle_day: empty summary, skipping")
		return nil
	}
	c, err := s.repos.Chronicle.Load(world)
	if err != nil {
		return fmt.Errorf("archive_chronicle_day: Chronicle.Load failed: %w", err)
	}
	if !c.AppendDay(day, summary) {
		s.log.Debug().Int("day", day).Msg("archive_chronicle_day: duplicate day, skipping")
		return nil
	}
	s.log.Info().Str("world", world).Int("day", day).Msg("archive_chronicle_day")
	if err := s.repos.Chronicle.Save(world, c); err != nil {
		return fmt.Errorf("archive_chronicle_day: Chronicle.Save failed: %w", err)
	}
	// Always run the compression hook. The hook
	// (ChronicleCompressWindow) is a no-op when:
	//   - no summarizer is wired (logs a warning),
	//   - the just-archived day is not on a window
	//     boundary AND no earlier window is unfilled,
	//   - the just-archived window is too thin to
	//     compress.
	if s.chronicleCompress != nil {
		if err := s.chronicleCompress(ctx, world, day); err != nil {
			s.log.Warn().
				Err(err).
				Str("world", world).
				Int("day", day).
				Msg("archive_chronicle_day: compress hook failed; will retry next call")
		}
	}
	// Bump state.md to the next day. The day counter
	// advances, inFlight=true, events cleared.
	if world != "" {
		st, err := s.repos.WorldState.Load(world)
		if err != nil {
			return fmt.Errorf("archive_chronicle_day: WorldState.Load failed: %w", err)
		}
		st.Day = day + 1
		st.InFlight = true
		st.Events = nil
		if err := s.repos.WorldState.Save(world, st); err != nil {
			return fmt.Errorf("archive_chronicle_day: WorldState.Save failed: %w", err)
		}
	}
	if s.worldStateInvalidate != nil {
		s.worldStateInvalidate("end_day")
	}
	return nil
}

// AppendEvent is a one-line wrapper that the dispatcher /
// GM can call instead of going through UpdateState.
func (s *State) AppendEvent(text string) error {
	info, err := s.repos.Info.Load()
	if err != nil || info.ActiveWorld == "" {
		return errors.New("state: no active world")
	}
	return s.repos.WorldState.AppendEvent(info.ActiveWorld, text)
}

// AppendHistoryToState appends a compaction summary as
// a dated history block to the world state.
func (s *State) AppendHistoryToState(world, summary string, at time.Time) error {
	if strings.TrimSpace(summary) == "" {
		return nil
	}
	if world == "" {
		return errors.New("state: world is empty")
	}
	st, err := s.repos.WorldState.Load(world)
	if err != nil {
		return fmt.Errorf("append_history_to_state: WorldState.Load failed: %w", err)
	}
	header := "[history сжато " + at.UTC().Format("2006-01-02 15:04") + " UTC]"
	st.Events = append(st.Events, header+"\n"+summary)
	if err := s.repos.WorldState.Save(world, st); err != nil {
		return fmt.Errorf("append_history_to_state: WorldState.Save failed: %w", err)
	}
	return nil
}

// EndSceneResult describes the state after end_scene.
type EndSceneResult struct {
	KeptNPCs      []string
	PrunedNPCsLen int
}

// EndScene closes the current scene without closing the
// day. Prunes the active roster to the permanent_party
// subset (or keeps the existing roster when permanentParty
// is nil).
func (s *State) EndScene(world string, permanentParty []string) (*EndSceneResult, error) {
	if world == "" {
		return nil, errors.New("end_scene: world is empty")
	}
	exists, err := s.repos.WorldState.Load(world)
	if err != nil {
		return nil, fmt.Errorf("end_scene: WorldState.Load failed: %w", err)
	}
	if permanentParty == nil {
		return &EndSceneResult{KeptNPCs: exists.NPCs, PrunedNPCsLen: 0}, nil
	}
	keep := make(map[string]struct{}, len(permanentParty))
	for _, n := range permanentParty {
		keep[strings.ToLower(strings.TrimSpace(n))] = struct{}{}
	}
	var newRoster []string
	for _, n := range exists.NPCs {
		if _, ok := keep[strings.ToLower(strings.TrimSpace(n))]; ok {
			newRoster = append(newRoster, n)
		}
	}
	pruned := len(exists.NPCs) - len(newRoster)
	exists.NPCs = newRoster
	if err := s.repos.WorldState.Save(world, exists); err != nil {
		return nil, fmt.Errorf("end_scene: WorldState.Save failed: %w", err)
	}
	return &EndSceneResult{KeptNPCs: newRoster, PrunedNPCsLen: pruned}, nil
}

// normaliseEventKey collapses an event string to a
// dedupe-friendly key by lowercasing and trimming.
func normaliseEventKey(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}
