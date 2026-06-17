package files

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/bestxp/narrative-ai-agent/internal/adapter/storage"
	"github.com/bestxp/narrative-ai-agent/internal/chronicle"
	"github.com/bestxp/narrative-ai-agent/internal/domain"
	"github.com/bestxp/narrative-ai-agent/internal/prompts"
	"github.com/bestxp/narrative-ai-agent/internal/slowlog"
	"github.com/bestxp/narrative-ai-agent/internal/usecase/tools"
)

// renderStateBody is the data-bag driven renderer for
// state.md. It replaces the old
// domain.BuildStateMarkdown(string-builder) path with
// the project's standard template-based pipeline. The
// template (prompts/state.md.tmpl) owns block order and
// conditional formatting; this helper only projects
// the in-memory StateSnapshot into the data-bag shape.
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

// State is the file-backed implementation of tools.StateTool:
// state.md, plan.md and chronicle.yaml lifecycle.
type State struct {
	fs  *storage.FileStore
	log zerolog.Logger
	// slow is the audit log; nil-safe (Write checks nil).
	// Wired at construction in NewFileToolset from the
	// process-level slowlog; tests pass slowlog.Discard().
	// Used to emit `tool.update_state` events with
	// npcs_added / npcs_removed / events_added deltas
	// so the operator (and the regression tests) can see
	// what an `update_state` tool call actually wrote
	// to state.md — the regular `c.log.Info` only emits
	// the day/in_flight/npcs/events counters, not the
	// per-element diff.
	slow *slowlog.Logger
	// chronicleCompress is invoked after a successful
	// chronicle append. The implementation lives on
	// *Memory (ChronicleCompressWindow). It is wired by
	// NewFileToolset so that the state writer does not
	// need a direct dependency on the memory struct.
	// The hook is expected to be nil-safe and to no-op
	// when no summarizer is wired.
	chronicleCompress func(ctx context.Context, world string, dayJustArchived int) error
	// worldStateInvalidate is invoked after the day is
	// closed (ArchiveChronicleDay, end_day). Wired by
	// NewFileToolset to call GM.InvalidateWorldState so
	// the next turn rebuilds index:1 from disk (the
	// "Протокол прошедших дней" section changed). It is
	// also invoked by /reload (dispatcher) and
	// leave_world (world.go).
	worldStateInvalidate func(reason string)
}

func newState(fs *storage.FileStore, log zerolog.Logger, slow *slowlog.Logger) *State {
	return &State{
		fs:   fs,
		log:  log.With().Str("component", "state").Logger(),
		slow: slow,
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

// StateHeader is the very first line of state.md per the skill rules.
func StateHeader(day int, inFlight bool) string {
	marker := "в процессе"
	if !inFlight {
		marker = "завершён"
	}
	return "День " + strconv.Itoa(day) + " (" + marker + ")."
}

// UpdateState writes a trimmed "here and now" snapshot. The
// хронология дня section grows by appending AppendEvents —
// one entry per call. StateSnapshot.AppendEvents is a delta,
// not a replacement, so concurrent /leave doesn't clobber the
// running log of the day.
func (s *State) UpdateState(snap tools.StateSnapshot) error {
	if snap.World == "" {
		snap.World = currentWorld(s.fs)
	}
	rel := "worlds/" + snap.World + "/state.md"
	cur, _ := s.fs.ReadRaw(rel)
	existing := ParseStateMD(cur)
	// Snapshot the BEFORE picture for the slowlog diff.
	// We compute npcs_added / npcs_removed / events_added
	// against the on-disk state, not against snap.NPCs /
	// snap.AppendEvents in isolation, so an `update_state`
	// call that doesn't change the roster (e.g. only updates
	// moment) is visible as npcs_added=[], npcs_removed=[],
	// rather than confusingly showing the entire roster
	// as "added".
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
		// Dedupe: a model that "obeys the rule" and appends
		// events every turn will inevitably repeat the
		// same beats ("Ирука ведёт в столовую" appears
		// three times if the GM only adds one new
		// sentence each call). Without this dedupe the
		// chronology grows by N events per turn and
		// repeats the same line N times. We compare on a
		// whitespace-normalised key so "Ирука повёл" and
		// "ирука повёл" collapse, and we skip empties.
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
	// Emit a structured `tool.update_state` slowlog event
	// with the per-element delta. The regular Info() above
	// tells the operator "this turn called update_state" —
	// this entry tells them WHAT it changed (which NPCs
	// joined or left the active roster, how many new
	// events were appended to the day's chronology).
	// Slowlog is nil-safe (Write guards against nil).
	//
	// npcs_added = elements in the new roster that
	// were NOT in the old roster (e.g. "Цунами",
	// "Инари" when the scene widens).
	//
	// npcs_removed = elements in the old roster that
	// are NOT in the new roster (e.g. "Какаши"
	// when he walks off-stage). Compute the diff as
	// `new \ old` (added) and `old \ new` (removed).
	if s.slow != nil {
		_ = s.slow.Write("tool.update_state", "", map[string]any{
			"day":          snap.Day,
			"in_flight":    snap.InFlight,
			"moment":       snap.Moment,
			"npcs_added":   diffStrings(existing.NPCs, prevNPCs),
			"npcs_removed": diffStrings(prevNPCs, existing.NPCs),
			"npcs_now":     existing.NPCs,
			"events_added": len(existing.Events) - prevEventCount,
			"path":         rel,
			"bytes":        len(body),
		})
	}
	return s.fs.WriteRawAtomic(rel, body)
}

// diffStrings returns elements present in `a` but not in `b`,
// preserving order. Used to compute npcs_added / npcs_removed
// for the `tool.update_state` slowlog event. Both inputs are
// treated as sets (case-sensitive, whitespace-trimmed).
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

// ParseStateMD is the inverse of BuildStateMarkdown — it
// recovers the StateSnapshot from a state.md body so UpdateState
// can append to the chronology without clobbering earlier events.
// We tolerate a missing "## Хронология дня" section (returns
// empty Events).
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

// RotatePlan replaces plan.md content. The "rotate" step means: caller
// passes the next 3-5 events, the past one is dropped.
func (s *State) RotatePlan(world string, events []string) error {
	if len(events) < 3 || len(events) > 5 {
		return &PlanRangeError{Given: len(events)}
	}
	var b strings.Builder
	b.WriteString("# План: " + world + "\n\n")
	for i, e := range events {
		b.WriteString("- День +")
		b.WriteString(strconv.Itoa(i + 1))
		b.WriteString(": ")
		b.WriteString(e)
		b.WriteString("\n")
	}
	return s.fs.WriteRawAtomic("worlds/"+world+"/plan.md", b.String())
}

// PlanRangeError signals that the caller passed a non-canonical number
// of upcoming events.
type PlanRangeError struct{ Given int }

func (e *PlanRangeError) Error() string {
	return "plan.md must contain 3-5 events, got " + strconv.Itoa(e.Given)
}

// ArchiveChronicleDay appends a new day entry to the
// world's chronicle, then triggers the
// chronicle-compression hook whenever a window closes.
// The compression hook is wired by NewFileToolset to
// Memory.ChronicleCompressWindow.
//
// Window rule (default Window=30, configurable in code):
// the hook is called when the day JustArchived equals
// `lastDay + Window - 1 + 1` modulo Window's multiplier —
// in plain terms, any day that is a multiple of Window
// (30, 60, 90, ...) closes the previous window. Wider
// timeskips are handled by ChronicleCompressWindow
// itself: if the last raw day in the file is, say, 10
// and we just archived 90, the hook will collapse
// days 1..30, 31..60, and 61..90 in three separate
// LLM calls.
func (s *State) ArchiveChronicleDay(ctx context.Context, world string, day int, summary string) error {
	if strings.TrimSpace(summary) == "" {
		s.log.Debug().Int("day", day).Msg("archive_chronicle_day: empty summary, skipping")
		return nil
	}
	rel := s.fs.WorldChronicle(world)
	raw, err := s.fs.ReadRaw(rel)
	if err != nil {
		return err
	}
	c, err := loadOrEmptyChronicle(raw)
	if err != nil {
		return err
	}
	if !c.AppendDay(day, summary) {
		s.log.Debug().Int("day", day).Msg("archive_chronicle_day: duplicate day, skipping")
		return nil
	}
	body, err := c.Save()
	if err != nil {
		return err
	}
	s.log.Info().Str("world", world).Int("day", day).Msg("archive_chronicle_day")
	if err := s.fs.WriteRawAtomic(rel, body); err != nil {
		return err
	}
	// Always run the compression hook. The hook
	// (ChronicleCompressWindow) is a no-op when:
	//   - no summarizer is wired (logs a warning),
	//   - the just-archived day is not on a window
	//     boundary AND no earlier window is unfilled
	//     (i.e. nothing to collapse),
	//   - the just-archived window is too thin to
	//     compress (e.g. only 3 real days of activity
	//     inside a 30-day window).
	// We never gate on a flag from the caller — a missed
	// window must not silently fall through, because the
	// LLM has no other chance to compress the data.
	if s.chronicleCompress != nil {
		if err := s.chronicleCompress(ctx, world, day); err != nil {
			// Compression is best-effort. The day
			// entry is already on disk, so a failed
			// compression does not lose data — the
			// NEXT ArchiveChronicleDay will
			// re-evaluate and collapse the open
			// window. Log and continue, do not
			// surface the error to the player (they
			// would see "end_day failed" for a
			// maintenance hiccup).
			s.log.Warn().
				Err(err).
				Str("world", world).
				Int("day", day).
				Msg("archive_chronicle_day: compress hook failed; will retry next call")
		}
	}
	if world != "" {
		st, _ := s.fs.ReadRaw("worlds/" + world + "/state.md")
		parsed := ParseStateMD(st)
		parsed.Day = day + 1
		parsed.InFlight = true
		parsed.Events = nil
		body, err := renderStateBody(parsed)
		if err != nil {
			return err
		}
		if err := s.fs.WriteRawAtomic("worlds/"+world+"/state.md", body); err != nil {
			return err
		}
	}
	// End-of-day closes the scene. Drop the world-state
	// snapshot so the next turn rebuilds index:1 with the
	// freshly appended "## Протокол прошедших дней" section.
	// (ArchiveChronicleDay is the only place in the
	// production flow that does this — the dispatcher
	// /reload path calls GM.InvalidateWorldState
	// directly.)
	if s.worldStateInvalidate != nil {
		s.worldStateInvalidate("end_day")
	}
	return nil
}

// loadOrEmptyChronicle parses the raw YAML body and
// returns an empty Chronicle on parse error. The
// empty Chronicle is then valid for Save() — it
// round-trips to "periods: []\ndays: {}" and the
// operator sees an empty-but-canonical file on
// disk. We prefer this over propagating the parse
// error because ArchiveChronicleDay should never
// block a player's "end of day" — a malformed
// chronicle is a recovery problem, not a runtime
// problem.
func loadOrEmptyChronicle(raw string) (chronicle.Chronicle, error) {
	if strings.TrimSpace(raw) == "" {
		return chronicle.Chronicle{Periods: []chronicle.Period{}, Days: map[int]string{}}, nil
	}
	return chronicle.Load(raw)
}

// AppendEvent is a one-line wrapper that the dispatcher / GM
// can call instead of going through UpdateState when all it
// needs is to grow the хронология дня. The world is read
// from the registry; the day and moment are preserved.
func (s *State) AppendEvent(text string) error {
	world := currentWorld(s.fs)
	if world == "" {
		return errors.New("state: no active world")
	}
	rel := "worlds/" + world + "/state.md"
	cur, _ := s.fs.ReadRaw(rel)
	parsed := ParseStateMD(cur)
	parsed.World = world
	parsed.Events = append(parsed.Events, text)
	body, err := renderStateBody(parsed)
	if err != nil {
		return err
	}
	return s.fs.WriteRawAtomic(rel, body)
}

// AppendHistoryToState appends a compaction summary as a new
// section to state.md's "## Хронология дня" block. The section
// is dated so /start can show "what the summariser decided".
func (s *State) AppendHistoryToState(world, summary string, at time.Time) error {
	if strings.TrimSpace(summary) == "" {
		return nil
	}
	if world == "" {
		return errors.New("state: world is empty")
	}
	rel := "worlds/" + world + "/state.md"
	cur, _ := s.fs.ReadRaw(rel)
	header := "[history сжато " + at.UTC().Format("2006-01-02 15:04") + " UTC]"
	if cur != "" && !strings.HasSuffix(cur, "\n") {
		cur += "\n"
	}
	next := cur + header + "\n" + summary + "\n"
	return s.fs.WriteRawAtomic(rel, next)
}

// EndSceneResult describes the state after end_scene
// has been applied: how many NPCs were pruned from the
// active roster and what permanent_party stays. The
// dispatcher surfaces a one-line summary to the player.
type EndSceneResult struct {
	KeptNPCs      []string
	PrunedNPCsLen int
}

// EndScene closes the current scene. It is the manual
// "scene change" handle the player can pull when they
// leave a location / switch sub-plot, but the day is
// not over. EndScene:
//
//   - rewrites state.md with the active roster pruned
//     to the permanent_party subset (the people who
//     travel with the player across scenes). A
//     missing permanent_party line is treated as
//     "keep the existing roster as-is" — operators
//     who want a forced prune must add the line by
//     hand.
//   - does NOT touch chronicle.yaml, does NOT call
//     ArchiveChronicleDay. The day's conversations stay in
//     memory until end_day (or /reload).
//   - does NOT compress the current scene's dialogue
//     to state.md — that path is owned by the
//     in-place compaction (and end_day for the
//     "before / after" protocol). The end_scene
//     tool's job is to reset the active roster, not
//     to summarise.
//
// The caller (gm.dispatchOneTool) is responsible for
// also dropping the per-chat conversation history so
// the player re-starts with a clean dialogue, and for
// invalidating the world snapshot so the next turn
// rebuilds user[0] with the pruned roster.
func (s *State) EndScene(world string, permanentParty []string) (*EndSceneResult, error) {
	if world == "" {
		return nil, errors.New("end_scene: world is empty")
	}
	rel := "worlds/" + world + "/state.md"
	cur, _ := s.fs.ReadRaw(rel)
	if cur == "" {
		// No state file yet — nothing to prune. The
		// tool still returns a result so the caller can
		// proceed with conversation reset + snapshot
		// invalidation.
		return &EndSceneResult{}, nil
	}
	parsed := ParseStateMD(cur)
	// If permanentParty is nil (the tool was called
	// without a config) we keep the existing roster
	// unchanged. This is the safe default — it lets
	// the operator/player move to a new location
	// without losing background NPC context.
	if permanentParty == nil {
		return &EndSceneResult{KeptNPCs: parsed.NPCs, PrunedNPCsLen: 0}, nil
	}
	// Build the keep-set. Members of the permanent
	// party stay in the active roster; everything else
	// is dropped.
	keep := make(map[string]struct{}, len(permanentParty))
	for _, n := range permanentParty {
		keep[strings.ToLower(strings.TrimSpace(n))] = struct{}{}
	}
	var newRoster []string
	for _, n := range parsed.NPCs {
		if _, ok := keep[strings.ToLower(strings.TrimSpace(n))]; ok {
			newRoster = append(newRoster, n)
		}
	}
	pruned := len(parsed.NPCs) - len(newRoster)
	parsed.NPCs = newRoster
	// Re-render. The existing moment/location/chronicle
	// are preserved — end_scene is a roster edit, not
	// a state reset.
	body, err := renderStateBody(parsed)
	if err != nil {
		return nil, err
	}
	if err := s.fs.WriteRawAtomic(rel, body); err != nil {
		return nil, err
	}
	return &EndSceneResult{KeptNPCs: newRoster, PrunedNPCsLen: pruned}, nil
}

// normaliseEventKey collapses an event string to a
// dedupe-friendly key by lowercasing and trimming
// surrounding whitespace. We do not strip inner
// punctuation because the same beat phrased slightly
// differently (e.g. "Ирука привёл" vs "Ирука повёл")
// should NOT dedupe — those are real narrative events.
// The key is intentionally narrow to catch only
// byte-for-byte duplicates modulo trivial whitespace.
func normaliseEventKey(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

func currentWorld(fs *storage.FileStore) string {
	info, _ := fs.ReadRaw(storage.InfoFile)
	if info == "" {
		return ""
	}
	parsed, err := domain.ParseInfo(info)
	if err != nil {
		return ""
	}
	return parsed.ActiveWorld
}
