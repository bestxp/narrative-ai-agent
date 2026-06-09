package files

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/rs/zerolog"

	"github.com/bestxp/narrative-ai-agent/internal/adapter/storage"
	"github.com/bestxp/narrative-ai-agent/internal/slowlog"
	"github.com/bestxp/narrative-ai-agent/internal/usecase/tools"
)

// Character is the file-backed implementation of
// tools.CharacterTool: SOUL/SKILL/memory.md appends and the
// /me snapshot read.
type Character struct {
	fs   *storage.FileStore
	log  zerolog.Logger
	slow *slowlog.Logger
	// world returns the active world name. Indirected through
	// a function so callers can pass a custom resolver (the
	// tests do this), and so a future in-process swap of the
	// active world does not require re-constructing the
	// toolset.
	world func() string
}

func newCharacter(fs *storage.FileStore, log zerolog.Logger, slow *slowlog.Logger) *Character {
	return &Character{
		fs:   fs,
		log:  log.With().Str("component", "character").Logger(),
		slow: slow,
		world: func() string {
			info, _ := fs.ReadRaw(storage.InfoFile)
			if info == "" {
				return ""
			}
			// Avoid an import cycle on domain by reusing
			// the parse path through the World helper below
			// — currentWorld is in state.go of this package.
			return currentWorld(fs)
		},
	}
}

// Public errors so callers can errors.Is against them without
// importing this package's internals.
var (
	ErrUnknownCharacterFile = errors.New("character: file must be SOUL, SKILL or memory")
	ErrEmptySection         = errors.New("character: section must not be empty")
	ErrEmptyAppend          = errors.New("character: append must not be empty")
	ErrNoActiveCharacter    = errors.New("character: no active character in registry")
)

// Append routes the new fact to characters/<name>/<file> with
// the section heading preserved.
func (c *Character) Append(characterDir, file, section, appendText string) error {
	if characterDir == "" {
		return ErrNoActiveCharacter
	}
	switch strings.ToLower(file) {
	case "soul":
		file = "SOUL.md"
	case "skill":
		file = "SKILL.md"
	case "memory":
		file = "memory.md"
	default:
		return ErrUnknownCharacterFile
	}
	if strings.TrimSpace(section) == "" {
		return ErrEmptySection
	}
	if strings.TrimSpace(appendText) == "" {
		return ErrEmptyAppend
	}
	rel := "characters/" + characterDir + "/" + file
	before, _ := c.fs.ReadRaw(rel)
	next := upsertSection(before, section, appendText)
	if err := c.fs.WriteRawAtomic(rel, next); err != nil {
		return err
	}
	c.log.Info().
		Str("character", characterDir).
		Str("file", file).
		Str("section", section).
		Int("bytes_added", len(appendText)).
		Msg("character_update")
	if c.slow != nil {
		_ = c.slow.Write("character.update", "", map[string]any{
			"character": characterDir,
			"file":      file,
			"section":   section,
			"appended":  appendText,
		})
	}
	return nil
}

func upsertSection(body, section, appendText string) string {
	header := "## " + section
	lines := strings.Split(body, "\n")
	headerIdx := -1
	for i, ln := range lines {
		if strings.TrimSpace(ln) == header {
			headerIdx = i
			break
		}
	}
	if headerIdx < 0 {
		if body != "" && !strings.HasSuffix(body, "\n") {
			body += "\n"
		}
		if body != "" {
			body += "\n"
		}
		return body + header + "\n" + strings.TrimSpace(appendText) + "\n"
	}
	endIdx := len(lines)
	for j := headerIdx + 1; j < len(lines); j++ {
		if strings.HasPrefix(strings.TrimSpace(lines[j]), "## ") {
			endIdx = j
			break
		}
	}
	// State sections REPLACE (current appearance,
	// weapons, philosophy); log sections APPEND
	// (memory, actions, preferences). See
	// classifySection for the canonical list.
	mode := classifySection(section)
	if mode == sectionModeState {
		return replaceSection(body, section, appendText, lines, headerIdx, endIdx)
	}
	// Log section: keep existing body, dedup
	// against appendText, then append the new line.
	tail := make([]string, 0, endIdx-headerIdx+1)
	for _, ln := range lines[headerIdx:endIdx] {
		if strings.TrimSpace(ln) == strings.TrimSpace(appendText) {
			return body
		}
		tail = append(tail, ln)
	}
	tail = append(tail, strings.TrimSpace(appendText))
	// Stitch: lines BEFORE the header, the section
	// body (header at tail[0]), then everything AFTER.
	// The earlier implementation emitted lines[:endIdx]
	// instead of lines[:headerIdx] and double-counted
	// the header — producing duplicate `## <section>`
	// headers on every Append.
	out := make([]string, 0, len(lines)+1)
	out = append(out, lines[:headerIdx]...)
	out = append(out, tail...)
	if endIdx < len(lines) {
		out = append(out, lines[endIdx:]...)
	}
	return strings.Join(out, "\n")
}

// sectionMode enumerates the two update semantics
// for a per-character file. State sections REPLACE
// their body (snapshot of "what I am right now"),
// log sections APPEND (journal). Unknown names
// default to ModeLog.
type sectionMode int

const (
	sectionModeLog   sectionMode = iota // append journal
	sectionModeState                    // replace snapshot
)

// stateSectionNames is the canonical list of
// section names that describe the character's
// CURRENT state. Updates land as REPLACE — a player
// who switched costumes does not want the old and
// new description stacked. Anything not on this
// list defaults to ModeLog (journal APPEND).
// All matches are case-insensitive. The split:
//
//   SOUL.md:  истинная сущность, истинный возраст,
//             визуальный возраст, внешний вид,
//             особое свойство, философия и принципы,
//             личностные качества, главная цель,
//             статус, сильные стороны, слабые стороны
//   SKILL.md: ранг, оружие, базовые способности,
//             фундаментальные стихии, особые проявления,
//             универсальные навыки, ограничения,
//             глаза, доспех
//   memory.md state-flavour: текущий статус, эмоции
var stateSectionNames = []string{
	// SOUL.md
	"истинная сущность",
	"истинный возраст",
	"визуальный возраст",
	"внешний вид",
	"особое свойство",
	"философия и принципы",
	"личностные качества",
	"главная цель",
	"статус",
	"сильные стороны",
	"слабые стороны",
	// SKILL.md
	"ранг",
	"оружие",
	"базовые способности",
	"фундаментальные стихии",
	"особые проявления",
	"универсальные навыки",
	"ограничения",
	"глаза",
	"доспех",
	// memory.md — state-flavoured snapshots
	"текущий статус",
	"эмоции",
}

func classifySection(section string) sectionMode {
	key := strings.ToLower(strings.TrimSpace(section))
	for _, n := range stateSectionNames {
		if n == key {
			return sectionModeState
		}
	}
	return sectionModeLog
}

// replaceSection rebuilds the file body with the
// section at [headerIdx, endIdx) replaced by a
// fresh header + the new text. The old body is
// dropped (the player is changing state, not adding
// to it). For a not-yet-written section this is
// identical to the "new section" branch in
// upsertSection. No dedup against the previous body
// — REPLACE is by definition saying "the old state
// is no longer current".
func replaceSection(body, section, appendText string, lines []string, headerIdx, endIdx int) string {
	newBody := strings.TrimSpace(appendText)
	if newBody == "" {
		return body
	}
	out := make([]string, 0, len(lines)+2)
	out = append(out, lines[:headerIdx]...)
	out = append(out, "## "+section)
	out = append(out, newBody)
	// Keep a blank line between the new body and the
	// next section so a reader can tell where this
	// section ends and the next one starts.
	out = append(out, "")
	if endIdx < len(lines) {
		rest := lines[endIdx:]
		if len(rest) > 0 && strings.TrimSpace(rest[0]) == "" {
			rest = rest[1:]
		}
		out = append(out, rest...)
	}
	return strings.Join(out, "\n")
}

// ExtractSections returns the ordered, deduplicated
// list of `## <name>` headers in body. Casing is
// preserved so the prompt surfaces the exact
// spellings the operator used — the model does not
// invent variants that get a duplicate-header file.
func ExtractSections(body string) []string {
	if strings.TrimSpace(body) == "" {
		return nil
	}
	var (
		out   []string
		seen  = map[string]struct{}{}
	)
	for _, ln := range strings.Split(body, "\n") {
		t := strings.TrimSpace(ln)
		if !strings.HasPrefix(t, "## ") {
			continue
		}
		name := strings.TrimSpace(t[3:])
		if name == "" {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out
}

// FormatSectionList renders the per-character
// "available sections" block the GM injects into
// the system prompt. Plain text so an operator can
// grep it when an `update_character` lands in the
// wrong place.
func FormatSectionList(soul, skill, mem string) string {
	var b strings.Builder
	any := false
	for _, sec := range []struct {
		heading string
		body    string
	}{
		{"SOUL.md", soul},
		{"SKILL.md", skill},
		{"memory.md", mem},
	} {
		names := ExtractSections(sec.body)
		if len(names) == 0 {
			continue
		}
		any = true
		fmt.Fprintf(&b, "### %s\n", sec.heading)
		for _, n := range names {
			fmt.Fprintf(&b, "- %s\n", n)
		}
		b.WriteString("\n")
	}
	if !any {
		return ""
	}
	return strings.TrimRight(b.String(), "\n")
}

// Read returns the snapshot of the current character.
func (c *Character) Read(activeChar, activeWorld string) (*tools.CharacterSnapshot, error) {
	if activeChar == "" {
		return nil, ErrNoActiveCharacter
	}
	soul, _ := c.fs.ReadRaw("characters/" + activeChar + "/SOUL.md")
	skill, _ := c.fs.ReadRaw("characters/" + activeChar + "/SKILL.md")
	mem, _ := c.fs.ReadRaw("characters/" + activeChar + "/memory.md")
	var state string
	var day int
	if activeWorld != "" {
		state, _ = c.fs.ReadRaw("worlds/" + activeWorld + "/state.md")
		day, _ = extractDayNumber(state)
	}
	return &tools.CharacterSnapshot{
		Character: activeChar,
		World:     activeWorld,
		SOUL:      soul,
		SKILL:     skill,
		Memory:    mem,
		State:     state,
		Day:       day,
	}, nil
}

// FormatSnapshot renders a snapshot for /me. Caps body sizes to keep
// the Telegram message under the 4096-char limit and to avoid dumping
// a multi-thousand-line lore file on a status check.
//
// Kept as a package-level function rather than a method on
// tools.CharacterSnapshot so the interface type stays free of
// presentation concerns. Callers (dispatcher.cmdMe) do
// snap.Format(40).
func FormatSnapshot(s *tools.CharacterSnapshot, maxPerSection int) string {
	if s == nil {
		return "Нет активного персонажа. Запустите /launch."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "**Персонаж: %s**\n", s.Character)
	if s.World != "" {
		fmt.Fprintf(&b, "**Мир: %s** (день %d)\n\n", s.World, s.Day)
	} else {
		b.WriteString("**Мир: —**\n\n")
	}
	for _, sec := range []struct {
		title string
		body  string
	}{
		{"SOUL.md (сущность)", s.SOUL},
		{"SKILL.md (навыки)", s.SKILL},
		{"memory.md (межмировые воспоминания)", s.Memory},
		{"state.md (текущий момент)", s.State},
	} {
		if sec.body == "" {
			fmt.Fprintf(&b, "## %s\n_(пусто)_\n\n", sec.title)
			continue
		}
		fmt.Fprintf(&b, "## %s\n", sec.title)
		b.WriteString(truncateForMe(sec.body, maxPerSection))
		b.WriteString("\n\n")
	}
	return strings.TrimSpace(b.String())
}

func truncateForMe(s string, max int) string {
	if max <= 0 {
		return s
	}
	lines := strings.Split(s, "\n")
	if len(lines) <= max {
		return s
	}
	return strings.Join(lines[:max], "\n") + fmt.Sprintf("\n[…+%d строк обрезано…]", len(lines)-max)
}

// extractDayNumber parses the "День N" line out of a
// state.md body. Returns (n, true) on hit, (0, false) when
// the marker is missing — callers can fall back to "day 1"
// without surfacing an error.
//
// Duplicated from the State struct's private helper so each
// file is self-contained; the regex is small.
var dayHeaderRe = regexp.MustCompile(`День (\d+)`)

func extractDayNumber(s string) (int, bool) {
	m := dayHeaderRe.FindStringSubmatch(s)
	if len(m) < 2 {
		return 0, false
	}
	n := 0
	for _, r := range m[1] {
		if r < '0' || r > '9' {
			return 0, false
		}
		n = n*10 + int(r-'0')
	}
	return n, true
}

// ExtractDayNumber is the public alias for the day-number
// parser; world-transition code calls it through this
// exported wrapper.
func ExtractDayNumber(s string) (int, bool) {
	return extractDayNumber(s)
}
