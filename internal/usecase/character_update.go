// Package usecase — application-level orchestration. The character
// update, GM, maintenance and world transition usecases all share
// this package and depend only on the storage / domain / slowlog
// adapters below them.
package usecase

import (
	"errors"
	"fmt"
	"strings"

	"github.com/rs/zerolog"

	"narrative/internal/adapter/storage"
	"narrative/internal/slowlog"
)

// SlowLogger is a thin alias so usecase callers do not have to
// import the slowlog package directly. The interface is satisfied
// by *slowlog.Logger.
type SlowLogger = slowlog.Logger

// CharacterUpdate persists new facts about the player character
// into the matching file under characters/<name>/. It is the
// counterpart of FirstLaunch.writeCharacter for the *runtime*
// phase: a fact the player revealed in the latest scene (name,
// age, a learned skill) is appended to the right section so it
// survives across sessions and across world transitions.
//
// Sections are looked up by their `## Header` line. If a section
// does not exist yet, it is appended to the file with a
// placeholder body. Existing content is never overwritten —
// the skill mandates forward-only files.
type CharacterUpdate struct {
	fs     *storage.FileStore
	log    zerolog.Logger
	slow   *slowlog.Logger
	world  func() string
}

func NewCharacterUpdate(fs *storage.FileStore, log zerolog.Logger, slow *slowlog.Logger) *CharacterUpdate {
	return NewCharacterUpdateWithWorld(fs, log, slow, func() string { return currentWorld(fs) })
}

func NewCharacterUpdateWithWorld(fs *storage.FileStore, log zerolog.Logger, slow *slowlog.Logger, worldFn func() string) *CharacterUpdate {
	return &CharacterUpdate{fs: fs, log: log.With().Str("component", "character_update").Logger(), slow: slow, world: worldFn}
}

var (
	ErrUnknownCharacterFile = errors.New("character_update: file must be SOUL, SKILL or memory")
	ErrEmptySection         = errors.New("character_update: section must not be empty")
	ErrEmptyAppend          = errors.New("character_update: append must not be empty")
	ErrNoActiveCharacter    = errors.New("character_update: no active character in registry")
)

// Append routes the new fact to characters/<name>/<file> with
// the section heading preserved. characterDir is the bare
// directory name (e.g. "markus") — the caller is the GM, which
// already knows the active character.
func (c *CharacterUpdate) Append(characterDir, file, section, appendText string) error {
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

// upsertSection inserts appendText into the `## section` block of
// the file body, creating the block at the end if missing. The
// existing content is preserved verbatim — only the new line is
// added.
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
		// Create the section at the end of the file.
		if body != "" && !strings.HasSuffix(body, "\n") {
			body += "\n"
		}
		if body != "" {
			body += "\n"
		}
		return body + header + "\n" + strings.TrimSpace(appendText) + "\n"
	}
	// Find the end of the section: next "## " heading or end of file.
	endIdx := len(lines)
	for j := headerIdx + 1; j < len(lines); j++ {
		if strings.HasPrefix(strings.TrimSpace(lines[j]), "## ") {
			endIdx = j
			break
		}
	}
	// Append before endIdx, avoiding a duplicate of the same line.
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

// CharacterSnapshot is the read-only bundle the dispatcher returns
// to /me. It is pre-formatted for plain-text output; markdown
// sections pass through unchanged.
type CharacterSnapshot struct {
	Character string
	World     string
	SOUL      string
	SKILL     string
	Memory    string
	State     string
	Day       int
}

// Read returns the snapshot of the current character. character
// and world are the active_* fields from info.yaml; if either is
// empty the snapshot still works (just with empty file bodies).
func (c *CharacterUpdate) Read(activeChar, activeWorld string) (*CharacterSnapshot, error) {
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
	return &CharacterSnapshot{
		Character: activeChar,
		World:     activeWorld,
		SOUL:      soul,
		SKILL:     skill,
		Memory:    mem,
		State:     state,
		Day:       day,
	}, nil
}

// Format renders a snapshot for /me. Caps body sizes to keep the
// Telegram message under the 4096-char limit and to avoid dumping
// a multi-thousand-line lore file on a status check.
func (s *CharacterSnapshot) Format(maxPerSection int) string {
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
