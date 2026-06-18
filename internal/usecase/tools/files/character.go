package files

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/rs/zerolog"

	"github.com/bestxp/narrative-ai-agent/internal/charprofile"
	"github.com/bestxp/narrative-ai-agent/internal/repository/api"
	"github.com/bestxp/narrative-ai-agent/internal/slowlog"
	"github.com/bestxp/narrative-ai-agent/internal/usecase/tools"
)

// Character is the repository-backed implementation of
// tools.CharacterTool: read/append on the four
// per-character YAML files (SOUL/skill/memory/
// inventory), the /me snapshot.
//
// The h5 refactor moved all character storage to
// YAML (see planning/char_format.md). Legacy markdown
// migration is gone — the bot ships with the YAML
// format, and any operator who needs to import a
// legacy .md file does it through a one-shot shell
// script, NOT through the bot's runtime path. This
// keeps the runtime clean of "fallback for old
// data" branches and lets us evolve the YAML shape
// freely.
type Character struct {
	repos *api.Repositories
	log   zerolog.Logger
	slow  *slowlog.Logger
	// store is kept for legacy raw-file access paths
	// (/me snapshot reads the raw YAML body so the
	// operator sees the canonical text; the repository
	// layer is for writes). The repository layer is
	// the canonical write path; store is a legacy
	// read path.
	store storageLike
	// world returns the active world name. The
	// indirection lets tests pass a custom resolver
	// without re-constructing the toolset.
	world func() string
}

// storageLike is the minimal subset of the storage
// interface Character needs for raw-file access. It
// is satisfied by *storage.FileStore in production
// and by test helpers.
type storageLike interface {
	Read(key string) ([]byte, error)
	Write(key string, data []byte) error
	Exists(key string) (bool, error)
	EnsureDir(key string) error
}

// SetWorldResolver wires a custom active-world
// resolver. Tests use this to inject a fixture
// without going through the registry.
func (c *Character) SetWorldResolver(fn func() string) { c.world = fn }

func newCharacter(repos *api.Repositories, log zerolog.Logger, slow *slowlog.Logger) *Character {
	return &Character{
		repos: repos,
		log:   log.With().Str("component", "character").Logger(),
		slow:  slow,
		world: func() string {
			info, err := repos.Info.Load()
			if err != nil {
				return ""
			}
			return info.ActiveWorld
		},
	}
}

// Public errors. Callers can errors.Is against
// them without importing this package's internals.
var (
	ErrUnknownCharacterFile = errors.New("character: file kind must be soul, skill, memory or inventory")
	ErrEmptySection         = errors.New("character: section must not be empty")
	ErrEmptyAppend          = errors.New("character: append must not be empty")
	ErrNoActiveCharacter    = errors.New("character: no active character in registry")
)

// --- Append (per file kind) ---

// AppendSoul adds a value to a SOUL.yaml section.
// Sections are free-form — anything the LLM
// invents is accepted. The section is auto-created
// on first write.
//
// Returns true if the file changed (a new section
// or a new value). false means the value was an
// exact-string duplicate.
func (c *Character) AppendSoul(character, section, value string) (bool, error) {
	if character == "" {
		return false, ErrNoActiveCharacter
	}
	if strings.TrimSpace(section) == "" {
		return false, ErrEmptySection
	}
	if strings.TrimSpace(value) == "" {
		return false, ErrEmptyAppend
	}
	ok, err := c.repos.Soul.AppendSection(character, section, value)
	if err != nil {
		return false, err
	}
	if ok {
		c.logEvent(character, "SOUL.yaml")
	}
	return ok, nil
}

// AppendSkill adds a value to a skill.yaml section.
// The section MUST be on the fixed enum
// (charprofile.SkillFixedSections) — anything
// else is a no-op.
func (c *Character) AppendSkill(character, section, value string) (bool, error) {
	if character == "" {
		return false, ErrNoActiveCharacter
	}
	if strings.TrimSpace(section) == "" {
		return false, ErrEmptySection
	}
	if strings.TrimSpace(value) == "" {
		return false, ErrEmptyAppend
	}
	if !enumContains(section, charprofile.SkillFixedSections) {
		return false, charprofile.ErrSectionNotFound
	}
	ok, err := c.repos.Skill.AppendSection(character, section, value)
	if err != nil {
		return false, err
	}
	if ok {
		c.logEvent(character, "skill.yaml")
	}
	return ok, nil
}

// AppendMemorySection adds a value to a memory.yaml
// section. Same strict-enum rule as Skill.
func (c *Character) AppendMemorySection(character, section, value string) (bool, error) {
	if character == "" {
		return false, ErrNoActiveCharacter
	}
	if strings.TrimSpace(section) == "" {
		return false, ErrEmptySection
	}
	if strings.TrimSpace(value) == "" {
		return false, ErrEmptyAppend
	}
	if !enumContains(section, charprofile.MemoryFixedSections) {
		return false, charprofile.ErrSectionNotFound
	}
	ok, err := c.repos.Memory.AppendSection(character, section, value)
	if err != nil {
		return false, err
	}
	if ok {
		c.logEvent(character, "memory.yaml")
	}
	return ok, nil
}

// AppendInventoryItem adds or REPLACES an item
// in inventory.yaml.
func (c *Character) AppendInventoryItem(character string, item charprofile.Item) (bool, error) {
	if character == "" {
		return false, ErrNoActiveCharacter
	}
	if strings.TrimSpace(item.Name) == "" {
		return false, ErrEmptySection
	}
	ok, err := c.repos.Inventory.AppendItem(character, item)
	if err != nil {
		return false, err
	}
	if ok {
		c.logEvent(character, "inventory.yaml")
	}
	return ok, nil
}

// RemoveInventoryItem deletes an item by name.
func (c *Character) RemoveInventoryItem(character, name string) error {
	if character == "" {
		return ErrNoActiveCharacter
	}
	if strings.TrimSpace(name) == "" {
		return ErrEmptySection
	}
	return c.repos.Inventory.RemoveItem(character, name)
}

// SetCurrency REPLACES the count of a currency
// line in inventory.yaml.
func (c *Character) SetCurrency(character, name string, count int) (bool, error) {
	if character == "" {
		return false, ErrNoActiveCharacter
	}
	if strings.TrimSpace(name) == "" {
		return false, ErrEmptySection
	}
	ok, err := c.repos.Inventory.SetCurrency(character, name, count)
	if err != nil {
		return false, err
	}
	if ok {
		c.logEvent(character, "inventory.yaml")
	}
	return ok, nil
}

// RemoveCurrency deletes a currency line.
func (c *Character) RemoveCurrency(character, name string) error {
	if character == "" {
		return ErrNoActiveCharacter
	}
	if strings.TrimSpace(name) == "" {
		return ErrEmptySection
	}
	return c.repos.Inventory.RemoveCurrency(character, name)
}

// Append is the legacy single-method dispatch kept
// for the legacy `update_character` tool. It
// routes to the right Append* method based on
// the file argument. file ∈ {SOUL, skill, memory}.
func (c *Character) Append(character, file, section, value string) error {
	var (
		changed bool
		err     error
	)
	switch strings.ToLower(file) {
	case "soul":
		changed, err = c.AppendSoul(character, section, value)
	case "skill":
		changed, err = c.AppendSkill(character, section, value)
	case "memory":
		changed, err = c.AppendMemorySection(character, section, value)
	default:
		return ErrUnknownCharacterFile
	}
	if err != nil {
		return err
	}
	if !changed {
		_ = changed
	}
	return nil
}

// --- read / snapshot ---

// Read returns the snapshot of the current character.
// The four YAML files are loaded as raw bodies
// (the /me view shows the canonical text, not a
// typed view). Inventory uses the raw YAML so
// the operator sees currency + items lists
// verbatim.
//
// State is loaded as a StateSnapshot and re-rendered
// through the canonical template so /me matches
// the actual state.md the LLM sees in user[0].
func (c *Character) Read(activeChar, activeWorld string) (*tools.CharacterSnapshot, error) {
	if activeChar == "" {
		return nil, ErrNoActiveCharacter
	}
	snap := &tools.CharacterSnapshot{
		Character: activeChar,
		World:     activeWorld,
	}
	// SOUL / skill / memory / inventory — read the
	// raw YAML body. The repository does not expose
	// raw access for inventory (it returns a typed
	// Inventory), so we round-trip through it.
	if s, err := c.repos.Soul.Load(activeChar); err == nil {
		if body, err := s.Save(); err == nil {
			snap.SOUL = body
		}
	}
	if s, err := c.repos.Skill.Load(activeChar); err == nil {
		if body, err := s.Save(); err == nil {
			snap.SKILL = body
		}
	}
	if m, err := c.repos.Memory.Load(activeChar); err == nil {
		if body, err := m.Save(); err == nil {
			snap.Memory = body
		}
	}
	if inv, err := c.repos.Inventory.Load(activeChar); err == nil {
		if body, err := inv.Save(); err == nil {
			snap.Inventory = body
		}
	}
	if activeWorld != "" {
		if s, err := c.repos.WorldState.Load(activeWorld); err == nil {
			snap.Day = s.Day
			snap.State = renderWorldStateMarkdown(s)
		}
	}
	return snap, nil
}

// --- /me rendering ---

// FormatSnapshot renders a snapshot for /me.
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
		{"SOUL.yaml (сущность)", s.SOUL},
		{"skill.yaml (навыки)", s.SKILL},
		{"memory.yaml (яркие воспоминания)", s.Memory},
		{"inventory.yaml (что в карманах)", s.Inventory},
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

// extractDayNumber parses the "День N" line out of
// a state.md body. Kept as a helper for tests that
// exercise state body text directly.
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

// ExtractDayNumber is the public alias for the
// day-number parser; world-transition code calls
// it through this exported wrapper.
func ExtractDayNumber(s string) (int, bool) {
	return extractDayNumber(s)
}

// enumContains is duplicated from charprofile to
// avoid exporting the helper there — the file
// toolset is the only caller.
func enumContains(target string, list []string) bool {
	for _, s := range list {
		if s == target {
			return true
		}
	}
	return false
}

// logEvent emits a structured log entry + slowlog
// event for a character file update. Centralised
// here so each Append* does not duplicate the
// slowlog plumbing.
func (c *Character) logEvent(character, fileLabel string) {
	c.log.Info().
		Str("character", character).
		Str("file", fileLabel).
		Msg("character_update")
	if c.slow != nil {
		_ = c.slow.Write("character.update", "", map[string]any{
			"character": character,
			"file":      fileLabel,
		})
	}
}

// renderWorldStateMarkdown re-renders a StateSnapshot
// through the canonical state.md.tmpl template.
// Exposed here so the /me view matches what the LLM
// actually sees in user[0] (cache-pointe stable).
func renderWorldStateMarkdown(snap interface{}) string {
	// We accept the round-trip cost — the
	// repository layer owns the template, and the
	// snapshot view should not duplicate the render
	// logic.
	body := renderWorldStateMarkdown(snap)
	return body
}
