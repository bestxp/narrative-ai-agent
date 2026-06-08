package files

import (
	"errors"
	"regexp"
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
}

func newState(fs *storage.FileStore, log zerolog.Logger) *State {
	return &State{fs: fs, log: log.With().Str("component", "state").Logger()}
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

// ArchiveDay appends a new day entry to memorise.md (and
// compresses 30-day windows per the skill rules) and resets
// the state's хронология дня for the new day.
func (s *State) ArchiveDay(world string, day int, summary string) error {
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
	next = s.compressIfNeeded(next)
	s.log.Info().Str("world", world).Int("day", day).Msg("archive_day")
	if err := s.fs.WriteRawAtomic(rel, next); err != nil {
		return err
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

// compressIfNeeded collapses 30-entry windows into a single block.
var windowRe = regexp.MustCompile(`д(\d{5}):\s+(.+?)(?:\n|$)`)

func (s *State) compressIfNeeded(body string) string {
	entries, err := domain.ParseDays(body)
	if err != nil || len(entries) <= 30 {
		return body
	}
	head := entries[:30]
	tail := entries[30:]
	var b strings.Builder
	b.WriteString(domain.FormatDay(head[0].Number, "сводка 30 дней ("))
	b.WriteString(strconv.Itoa(len(head)))
	b.WriteString(" дн.) — удалено построчно\n")
	for _, e := range tail {
		b.WriteString(domain.FormatDay(e.Number, e.Text))
		b.WriteString("\n")
	}
	return b.String()
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
