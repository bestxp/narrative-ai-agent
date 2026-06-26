// Package chronicle is the canonical on-disk shape and
// in-memory representation of a world's day-by-day
// log + LLM-compressed period summaries.
//
// The legacy format was a flat markdown file with one
// line per day ("д<NNNNN>: <text>") and one line per
// compressed window ("д<a>-д<b>: <text>"). That worked
// for small files but made it impossible to add
// structured metadata per day (e.g. a "scene" tag) or
// to reason about windows without re-parsing the
// whole body.
//
// The new format is a small YAML document with two
// sections:
//
//	periods:                  # LLM-compressed windows
//	  - from: 1
//	    to: 30
//	    memory: "<distilled essence of days 1..30>"
//	  - from: 31
//	    to: 60
//	    memory: "..."
//	days:                     # raw per-day log
//	  1: "raw narrative for day 1"
//	  2: "raw narrative for day 2"
//
// Window-compression rule (unchanged from the legacy
// format): every time ArchiveDay records a day that
// closes a window of Window=30 days, the summarizer
// is called with the raw days in that window and
// emits a single Period entry that replaces the raw
// days.
//
// The on-disk file is YAML. The model and the
// dispatcher never see YAML — they see the rendered
// markdown from BuildMarkdown (chronicle.md.tmpl),
// which emits two clearly-delimited blocks:
//
//	### Воспоминания за периоды
//	с 1 по 30 дни: <memory>
//	с 31 по 60 дни: <memory>
//
//	### Последняя хронология событий
//	День 1: <text>
//	День 2: <text>
//	День 7: <text>
//
// Storage stays hidden behind Load / Save / AppendDay
// / CompressWindow — callers do not touch YAML
// directly.
package chronicle

import (
	"errors"
	"fmt"
	"maps"
	"sort"
	"strings"

	"github.com/bestxp/narrative-ai-agent/internal/limits"
	"gopkg.in/yaml.v3"
)

// FileStore is the minimal storage surface the chronicle
// package needs. Mirrors staging.FileStore so callers can
// pass a *storage.FileStore directly.
type FileStore interface {
	ReadRaw(rel string) (string, error)
	WriteRawAtomic(rel, body string) error
	Exists(rel string) bool
}

// Period is one LLM-compressed summary covering
// raw days [From..To], inclusive on both ends. The
// raw day entries for that range are removed from
// `days` once a Period is appended; the source of
// truth is always the Period entry (the summarizer
// might have re-phrased facts from the originals,
// and we do not keep both).
type Period struct {
	From   int    `yaml:"from"`
	To     int    `yaml:"to"`
	Memory string `yaml:"memory"`
}

// Chronicle is the in-memory representation of a world's
// chronicle file. Periods are kept sorted by From
// (ascending). Days are stored as map[int]string for O(1)
// lookup; iteration order is undefined by the language,
// so the renderer sorts by day when emitting.
type Chronicle struct {
	// Periods is the LLM-compressed window log. The
	// most recent period is the LAST element (new
	// periods are appended at the tail). For render
	// we iterate in From-order, which is also
	// append-order (the compression hook never
	// re-orders).
	//
	// No `omitempty` on the YAML tags: an empty
	// Chronicle MUST serialise to "periods: []" and
	// "days: {}" so AppendDay always sees a fresh
	// file shape, and so an operator who opens an
	// empty chronicle sees "yes, this is the canonical
	// shape, not just a placeholder".
	Periods []Period `yaml:"periods"`
	// Days is the raw per-day log for the current
	// open window (the most-recent uncompressed
	// days). Once a window closes, those day
	// entries are removed and a Period is added.
	Days map[int]string `yaml:"days"`
}

// ErrNotFound is returned by Load when the file does
// not exist or is empty. The dispatcher turns this
// into a no-op (a world with no chronicle is the
// default state — AppendDay will create the file).
var ErrNotFound = errors.New("chronicle: file not found or empty")

// chronicleFile is the on-disk YAML shape. We separate
// it from Chronicle so internal bookkeeping (the
// private ordering, future fields) does not leak into
// the file format.
type chronicleFile struct {
	Periods []periodYAML   `yaml:"periods"`
	Days    map[int]string `yaml:"days"`
}

type periodYAML struct {
	From   int    `yaml:"from"`
	To     int    `yaml:"to"`
	Memory string `yaml:"memory"`
}

// Load parses a chronicle YAML body. Returns ErrNotFound
// for an empty file; any other parse error is wrapped.
// A file that contains only one of the two sections
// (e.g. periods: [] but no days) is valid — both
// sections are optional.
func Load(body string) (Chronicle, error) {
	if strings.TrimSpace(body) == "" {
		return Chronicle{}, ErrNotFound
	}

	var raw chronicleFile
	if err := yaml.Unmarshal([]byte(body), &raw); err != nil {
		return Chronicle{}, fmt.Errorf("chronicle: yaml.Unmarshal: %w", err)
	}

	out := Chronicle{Periods: make([]Period, 0, len(raw.Periods))}
	for _, p := range raw.Periods {
		out.Periods = append(out.Periods, Period{
			From:   p.From,
			To:     p.To,
			Memory: strings.TrimSpace(p.Memory),
		})
	}

	if raw.Days != nil {
		out.Days = make(map[int]string, len(raw.Days))
		for k, v := range raw.Days {
			out.Days[k] = strings.TrimSpace(v)
		}
	}

	return out, nil
}

// Save serialises the chronicle back to disk in the
// canonical YAML shape. Periods are emitted in
// append-order; days are written in numeric-key
// order (yaml.Marshal of a map[int]string already
// sorts integer keys).
//
// Empty maps/slices are emitted as `days: {}` /
// `periods: []` rather than being omitted — a
// freshly-created chronicle is unambiguously
// "empty" rather than "missing the section". This
// matters for AppendDay, which always calls Save
// after mutation (the dispatcher can rely on the
// file existing post-call).
func (c *Chronicle) Save() (string, error) {
	out := chronicleFile{
		Periods: make([]periodYAML, 0, len(c.Periods)),
		Days:    map[int]string{},
	}
	for _, p := range c.Periods {
		out.Periods = append(out.Periods, periodYAML(p))
	}

	maps.Copy(out.Days, c.Days)

	body, err := yaml.Marshal(out)
	if err != nil {
		return "", fmt.Errorf("chronicle: yaml.Marshal: %w", err)
	}

	return string(body), nil
}

// SortedDays returns the raw day entries sorted by day
// number (ascending). Used by the renderer to emit a
// stable block; the map iteration order is otherwise
// non-deterministic.
func (c *Chronicle) SortedDays() []DayEntry {
	out := make([]DayEntry, 0, len(c.Days))
	for k, v := range c.Days {
		out = append(out, DayEntry{Number: k, Text: v})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })

	return out
}

// DayEntry is one raw day. Exported so the renderer
// (which lives in internal/prompts) can iterate the
// sorted list without re-implementing the sort.
type DayEntry struct {
	Number int
	Text   string
}

// LastDay returns the most-recent raw day number
// recorded in the open window. Returns (0, false)
// when no raw days are present (the world has either
// never been played or every window has been
// compressed). The checkSync helper in
// usecase/sessionstart.go relies on this to detect
// "chronicle ahead of state" drift.
func (c *Chronicle) LastDay() (int, bool) {
	if len(c.Days) == 0 {
		return 0, false
	}

	highest := 0
	for k := range c.Days {
		if k > highest {
			highest = k
		}
	}

	return highest, true
}

// LastPeriodEnd returns the highest "to" day across
// all closed periods (the most-recent compressed
// window's end). Used by the compression hook to
// know which days are already finalised and MUST
// NOT be touched. Returns (0, false) when no
// period has been written yet.
func (c *Chronicle) LastPeriodEnd() (int, bool) {
	if len(c.Periods) == 0 {
		return 0, false
	}

	m := 0
	for _, p := range c.Periods {
		if p.To > m {
			m = p.To
		}
	}

	return m, true
}

// AppendDay records a raw day entry. The caller's
// preconditions:
//
//   - day is positive and strictly greater than any
//     previously-recorded day (the dispatcher enforces
//     this by deriving day from the state-day counter
//   - 1 on each ArchiveChronicleDay call);
//   - text is non-empty (the dispatcher rejects empty
//     summaries before reaching this method).
//
// Duplicate days are silently dropped — a re-issue of
// the same ArchiveChronicleDay call (e.g. after a
// network blip) does not corrupt the log. Returns
// true when the entry was added.
func (c *Chronicle) AppendDay(day int, text string) bool {
	text = strings.TrimSpace(text)
	if text == "" || day <= 0 {
		return false
	}

	if c.Days == nil {
		c.Days = map[int]string{}
	}

	if _, exists := c.Days[day]; exists {
		return false
	}

	c.Days[day] = text

	return true
}

// CompressWindow collapses the raw day entries in
// [from..to] into a single Period entry with the given
// memory text. The raw entries for that range are
// REMOVED — the Period is the new source of truth for
// those days. Raw entries outside [from..to] are
// preserved untouched (an open window continues
// alongside closed ones).
//
// Preconditions:
//
//   - from <= to (the caller computes this from the
//     window-closing rule);
//   - the raw day set for [from..to] is non-empty
//     (the compression hook returns "no-op" when the
//     window is too thin before reaching this method);
//   - to is strictly greater than any previously-
//     closed period's end (we never compress into
//     history). When this is violated the new period
//     is rejected with an error and the chronicle is
//     left untouched.
//
// memory must already be validated by the caller
// (see usecase.SummarizeChronicle for the prefix
// contract — we strip leading/trailing whitespace
// but otherwise pass through).
func (c *Chronicle) CompressWindow(from, to int, memory string) error {
	if from > to || from <= 0 || to <= 0 {
		return fmt.Errorf("chronicle: CompressWindow: invalid range [%d..%d]", from, to)
	}

	memory = strings.TrimSpace(memory)
	if memory == "" {
		return errors.New("chronicle: CompressWindow: empty memory text")
	}

	if lastEnd, ok := c.LastPeriodEnd(); ok && from <= lastEnd {
		return fmt.Errorf("chronicle: CompressWindow: window start %d is inside closed period (last end %d)", from, lastEnd)
	}
	// Remove raw days in the range. Days outside the
	// range stay put.
	for k := range c.Days {
		if k >= from && k <= to {
			delete(c.Days, k)
		}
	}

	c.Periods = append(c.Periods, Period{
		From:   from,
		To:     to,
		Memory: memory,
	})

	return nil
}

// WindowSize is the canonical window for compression.
// The legacy constant was `const window = 30` in
// usecase/tools/files/memory.go. We re-export it from
// internal/limits so the LLM-side template and the
// Go-side dispatcher share the same value.
//
// The local alias is kept for back-compat with every
// existing caller in this package.
const WindowSize = limits.MemoriseWindowDays
