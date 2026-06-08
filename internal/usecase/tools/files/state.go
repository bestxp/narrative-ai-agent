package files

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/bestxp/narrative-ai-agent/internal/adapter/storage"
	"github.com/bestxp/narrative-ai-agent/internal/domain"
	"github.com/bestxp/narrative-ai-agent/internal/usecase/tools"
)

// State is the file-backed implementation of tools.StateTool:
// state.md, plan.md and memorise.md lifecycle.
type State struct {
	fs  *storage.FileStore
	log zerolog.Logger
	// memoriseCompress is invoked after a successful
	// memorise.md append. The implementation lives on
	// *Memory (MemoriseCompressWindow). It is wired by
	// NewFileToolset so that the state writer does not
	// need a direct dependency on the memory struct.
	// The hook is expected to be nil-safe and to no-op
	// when no summarizer is wired.
	memoriseCompress func(ctx context.Context, world string, dayJustArchived int) error
}

func newState(fs *storage.FileStore, log zerolog.Logger) *State {
	return &State{fs: fs, log: log.With().Str("component", "state").Logger()}
}

// SetMemoriseCompress wires the post-ArchiveDay hook.
// Called once at construction time from NewFileToolset.
func (s *State) SetMemoriseCompress(fn func(ctx context.Context, world string, dayJustArchived int) error) {
	s.memoriseCompress = fn
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
	existing := parseStateMD(cur)
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
	body := domain.BuildStateMarkdown(existing)
	s.log.Info().
		Int("day", snap.Day).
		Bool("in_flight", snap.InFlight).
		Int("npcs", len(snap.NPCs)).
		Int("events", len(existing.Events)).
		Msg("update_state")
	return s.fs.WriteRawAtomic(rel, body)
}

// parseStateMD is the inverse of BuildStateMarkdown — it
// recovers the StateSnapshot from a state.md body so UpdateState
// can append to the chronology without clobbering earlier events.
// We tolerate a missing "## Хронология дня" section (returns
// empty Events).
func parseStateMD(body string) domain.StateSnapshot {
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

// ArchiveDay appends a new day entry to memorise.md, then
// triggers the memorise-compression hook whenever a window
// closes. The compression hook is wired by NewFileToolset
// to Memory.MemoriseCompressWindow.
//
// Window rule (default Window=30, configurable in code):
// the hook is called when the day JustArchived equals
// `lastDay + Window - 1 + 1` modulo Window's multiplier —
// in plain terms, any day that is a multiple of Window
// (30, 60, 90, ...) closes the previous window. Wider
// timeskips are handled by MemoriseCompressWindow itself:
// if the last day in the file is, say, д00010 and we just
// archived д00090, the hook will collapse д00001-д00030,
// д00031-д00060, and д00061-д00090 in three separate
// LLM calls.
func (s *State) ArchiveDay(ctx context.Context, world string, day int, summary string) error {
	if strings.TrimSpace(summary) == "" {
		s.log.Debug().Int("day", day).Msg("archive_day: empty summary, skipping")
		return nil
	}
	rel := "worlds/" + world + "/memorise.md"
	current, err := s.fs.ReadRaw(rel)
	if err != nil {
		return err
	}
	line := domain.FormatDay(day, summary)
	if strings.Contains(current, line) {
		s.log.Debug().Int("day", day).Msg("archive_day: already present")
		return nil
	}
	if current != "" && !strings.HasSuffix(current, "\n") {
		current += "\n"
	}
	next := current + line + "\n"
	s.log.Info().Str("world", world).Int("day", day).Msg("archive_day")
	if err := s.fs.WriteRawAtomic(rel, next); err != nil {
		return err
	}
	// Always run the compression hook. The hook
	// (MemoriseCompressWindow) is a no-op when:
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
	if s.memoriseCompress != nil {
		if err := s.memoriseCompress(ctx, world, day); err != nil {
			// Compression is best-effort. The day
			// entry is already on disk, so a failed
			// compression does not lose data — the
			// NEXT ArchiveDay will re-evaluate and
			// collapse the open window. Log and
			// continue, do not surface the error to
			// the player (they would see "end_day
			// failed" for a maintenance hiccup).
			s.log.Warn().
				Err(err).
				Str("world", world).
				Int("day", day).
				Msg("archive_day: memorise compress hook failed; will retry next call")
		}
	}
	if world != "" {
		st, _ := s.fs.ReadRaw("worlds/" + world + "/state.md")
		parsed := parseStateMD(st)
		parsed.Day = day + 1
		parsed.InFlight = true
		parsed.Events = nil
		body := domain.BuildStateMarkdown(parsed)
		if err := s.fs.WriteRawAtomic("worlds/"+world+"/state.md", body); err != nil {
			return err
		}
	}
	return nil
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
	parsed := parseStateMD(cur)
	parsed.World = world
	parsed.Events = append(parsed.Events, text)
	body := domain.BuildStateMarkdown(parsed)
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
