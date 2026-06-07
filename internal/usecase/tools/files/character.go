package files

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/rs/zerolog"

	"narrative/internal/adapter/storage"
	"narrative/internal/slowlog"
	"narrative/internal/usecase/tools"
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
	tail := make([]string, 0, endIdx-headerIdx)
	for _, ln := range lines[headerIdx:endIdx] {
		if strings.TrimSpace(ln) == strings.TrimSpace(appendText) {
			return body
		}
		tail = append(tail, ln)
	}
	tail = append(tail, strings.TrimSpace(appendText))
	out := make([]string, 0, len(lines)+1)
	out = append(out, lines[:endIdx]...)
	out = append(out, tail...)
	if endIdx < len(lines) {
		out = append(out, lines[endIdx:]...)
	}
	return strings.Join(out, "\n")
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
