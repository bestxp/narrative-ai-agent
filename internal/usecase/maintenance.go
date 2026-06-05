package usecase

import (
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
type StateSnapshot struct {
	Day      int
	InFlight bool
	Moment   string
	NPCs     []string
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

// UpdateState writes a trimmed "here and now" snapshot.
func (m *Maintenance) UpdateState(snap StateSnapshot) error {
	var b strings.Builder
	b.WriteString(StateHeader(snap.Day, snap.InFlight))
	b.WriteString("\n")
	b.WriteString(strings.TrimSpace(snap.Moment))
	if b.Len() > 0 && !strings.HasSuffix(b.String(), "\n") {
		b.WriteString("\n")
	}
	if len(snap.NPCs) > 0 {
		b.WriteString("\nАктивные NPC прямо сейчас: ")
		b.WriteString(strings.Join(snap.NPCs, ", "))
		b.WriteString("\n")
	}
	m.log.Info().Int("day", snap.Day).Bool("in_flight", snap.InFlight).Int("npcs", len(snap.NPCs)).Msg("update_state")
	return m.fs.WriteRawAtomic("worlds/"+currentWorld(m.fs)+"/state.md", b.String())
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

// ArchiveDay appends a new day entry to memorise.md (and compresses
// 30-day windows per the skill rules).
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
	return m.fs.WriteRawAtomic(rel, next)
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
	info, _ := fs.ReadRaw("info.md")
	if info == "" {
		return ""
	}
	_, w, err := domain.ParseInfo(info)
	if err != nil {
		return ""
	}
	ptr := w.Pointer
	return strings.TrimPrefix(ptr, "worlds/")
}
