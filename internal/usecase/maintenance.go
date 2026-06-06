package usecase

import (
	"errors"
	"regexp"
	"strconv"
	"strings"

	"github.com/rs/zerolog"

	"narrative/internal/adapter/storage"
	"narrative/internal/domain"
)

// Maintenance encapsulates the "end of scene / end of day" checklist
// described in the lazy-universe skill. It is intentionally framework-
// agnostic: callers pass in a FileStore and the active world name.
type Maintenance struct {
	fs  *storage.FileStore
	log zerolog.Logger
}

func NewMaintenance(fs *storage.FileStore) *Maintenance {
	return NewMaintenanceWithLogger(fs, zerolog.Nop())
}

func NewMaintenanceWithLogger(fs *storage.FileStore, log zerolog.Logger) *Maintenance {
	return &Maintenance{fs: fs, log: log.With().Str("component", "maintenance").Logger()}
}

// Report is what callers render to the player after the checklist runs.
type Report struct {
	StateUpdated     bool
	NPCsCompacted    []string
	PlanRotated      bool
	DayArchived      bool
	LoreTouched      bool
	CanonTouched     bool
	MemoryTouched    bool
	GitCommitted     bool
	GitPushSucceeded bool
	Notes            []string
}

// StateSnapshot is the trimmed "here and now" written to state.md.
// AppendEvents is the хронология дня section: each entry is
// appended on every UpdateState call (and the section is
// cleared on ArchiveDay).
type StateSnapshot struct {
	World        string
	Day          int
	InFlight     bool
	Location     string
	Moment       string
	NPCs         []string
	AppendEvents []string
}

// CompactRule describes when an NPC file should be condensed.
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
func (m *Maintenance) UpdateState(snap StateSnapshot) error {
	if snap.World == "" {
		snap.World = currentWorld(m.fs)
	}
	// Load the existing state to preserve the chronology.
	rel := "worlds/" + snap.World + "/state.md"
	cur, _ := m.fs.ReadRaw(rel)
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
		existing.Events = append(existing.Events, snap.AppendEvents...)
	}
	body := domain.BuildStateMarkdown(existing)
	m.log.Info().
		Int("day", snap.Day).
		Bool("in_flight", snap.InFlight).
		Int("npcs", len(snap.NPCs)).
		Int("events", len(existing.Events)).
		Msg("update_state")
	return m.fs.WriteRawAtomic(rel, body)
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
			// "День N (в процессе|завершён)."
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
func (m *Maintenance) RotatePlan(world string, events []string) error {
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
	return m.fs.WriteRawAtomic("worlds/"+world+"/plan.md", b.String())
}

// PlanRangeError signals that the caller passed a non-canonical number
// of upcoming events.
type PlanRangeError struct{ Given int }

func (e *PlanRangeError) Error() string {
	return "plan.md must contain 3-5 events, got " + strconv.Itoa(e.Given)
}

// ArchiveDay appends a new day entry to memorise.md (and
// compresses 30-day windows per the skill rules) and resets
// the state's хронология дня for the new day. The moment
// (current narrative beat) is left intact — only the
// day-by-day log moves to memorise.
func (m *Maintenance) ArchiveDay(world string, day int, summary string) error {
	if strings.TrimSpace(summary) == "" {
		m.log.Debug().Int("day", day).Msg("archive_day: empty summary, skipping")
		return nil // empty days are skipped per skill
	}
	rel := "worlds/" + world + "/memorise.md"
	current, err := m.fs.ReadRaw(rel)
	if err != nil {
		return err
	}
	line := domain.FormatDay(day, summary)
	if strings.Contains(current, line) {
		m.log.Debug().Int("day", day).Msg("archive_day: already present")
		return nil
	}
	if current != "" && !strings.HasSuffix(current, "\n") {
		current += "\n"
	}
	next := current + line + "\n"
	next = m.compressIfNeeded(next)
	m.log.Info().Str("world", world).Int("day", day).Msg("archive_day")
	if err := m.fs.WriteRawAtomic(rel, next); err != nil {
		return err
	}
	// Reset state's day chronology: day = day+1, in_flight = true,
	// events cleared. The "Момент" stays — the player just woke up
	// somewhere.
	if world != "" {
		st, _ := m.fs.ReadRaw("worlds/" + world + "/state.md")
		parsed := parseStateMD(st)
		parsed.Day = day + 1
		parsed.InFlight = true
		parsed.Events = nil
		body := domain.BuildStateMarkdown(parsed)
		if err := m.fs.WriteRawAtomic("worlds/"+world+"/state.md", body); err != nil {
			return err
		}
	}
	return nil
}

// compressIfNeeded collapses 30-entry windows into a single block.
var windowRe = regexp.MustCompile(`д(\d{5}):\s+(.+?)(?:\n|$)`)

func (m *Maintenance) compressIfNeeded(body string) string {
	entries, err := domain.ParseDays(body)
	if err != nil || len(entries) <= 30 {
		return body
	}
	// Find first 30 entries and replace with one summary line.
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

// AppendEvent is a one-line wrapper that the dispatcher / GM
// can call instead of going through UpdateState when all it
// needs is to grow the хронология дня. The world is read
// from the registry; the day and moment are preserved.
func (m *Maintenance) AppendEvent(text string) error {
	world := currentWorld(m.fs)
	if world == "" {
		return errors.New("maintenance: no active world")
	}
	rel := "worlds/" + world + "/state.md"
	cur, _ := m.fs.ReadRaw(rel)
	parsed := parseStateMD(cur)
	parsed.World = world
	parsed.Events = append(parsed.Events, text)
	body := domain.BuildStateMarkdown(parsed)
	return m.fs.WriteRawAtomic(rel, body)
}

// AppendLore appends a new deviation entry to lore.md.
func (m *Maintenance) AppendLore(world, header, bullet string) error {
	rel := "worlds/" + world + "/lore.md"
	cur, _ := m.fs.ReadRaw(rel)
	if cur != "" && !strings.HasSuffix(cur, "\n") {
		cur += "\n"
	}
	cur += "\n## " + header + "\n- " + bullet + "\n"
	return m.fs.WriteRawAtomic(rel, cur)
}

// AppendMemory appends a single first-person line to the active
// character's memory.md.
func (m *Maintenance) AppendMemory(character, line string) error {
	rel := "characters/" + character + "/memory.md"
	_, err := m.fs.AppendIfMissing(rel, "- "+strings.TrimSpace(line))
	return err
}

// CompactNPCs walks characters/*.md and condenses any file longer than
// the configured threshold. The actual condensing is delegated to
// NPCCompactor (see npc.go).
func (m *Maintenance) CompactNPCs(world string) ([]string, error) {
	dir := "worlds/" + world + "/characters"
	files, err := m.fs.ListChildren(dir)
	if err != nil {
		return nil, err
	}
	var touched []string
	for _, f := range files {
		if !strings.HasSuffix(f, ".md") {
			continue
		}
		rel := dir + "/" + f
		if m.fs.CountLines(rel) <= NPCCompactLineThreshold {
			continue
		}
		body, _ := m.fs.ReadRaw(rel)
		condensed := CompactNPCBody(body)
		if err := m.fs.WriteRawAtomic(rel, condensed); err != nil {
			return touched, err
		}
		touched = append(touched, strings.TrimSuffix(f, ".md"))
	}
	m.log.Info().Str("world", world).Strs("npcs", touched).Msg("compact_npcs")
	return touched, nil
}

// --- helpers ---

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
