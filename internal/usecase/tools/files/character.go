package files

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/bestxp/narrative-ai-agent/internal/charprofile"
	"github.com/bestxp/narrative-ai-agent/internal/repository/api"
	"github.com/bestxp/narrative-ai-agent/internal/slowlog"
	"github.com/bestxp/narrative-ai-agent/internal/usecase/tools"
	"github.com/rs/zerolog"
)

// Character is the repository-backed implementation of
// tools.CharacterTool: read/append on the four
// per-character YAML files (SOUL/skill/memory/
// inventory), the /me snapshot.
//
// The h5 refactor moved all character storage to
// YAML (see planning/char_format.md). The bot
// ships with the YAML format and evolves the
// on-disk shape freely — there is no runtime
// fallback for old data. Operators migrating from
// a legacy format run a one-shot shell script
// outside the bot's runtime path.
type Character struct {
	Repos *api.Repositories
	log   zerolog.Logger
	slow  *slowlog.Logger
	// world returns the active world name. The
	// indirection lets tests pass a custom resolver
	// without re-constructing the toolset.
	world func() string
}

// NewCharacter constructs a Character tool backed by the
// supplied repositories. The active world is resolved on
// demand through repos.Info.Load(); tests can override
// the resolver via SetWorldResolver.
func NewCharacter(repos *api.Repositories, log zerolog.Logger, slow *slowlog.Logger) *Character {
	return &Character{
		Repos: repos,
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

// SetWorldResolver wires a custom active-world
// resolver. Tests use this to inject a fixture
// without going through the registry.
func (c *Character) SetWorldResolver(fn func() string) { c.world = fn }

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

	ok, err := c.Repos.Soul.AppendSection(character, section, value)
	if err != nil {
		return false, fmt.Errorf("wrap: %w", err)
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

	ok, err := c.Repos.Skill.AppendSection(character, section, value)
	if err != nil {
		return false, fmt.Errorf("wrap: %w", err)
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

	ok, err := c.Repos.Memory.AppendSection(character, section, value)
	if err != nil {
		return false, fmt.Errorf("wrap: %w", err)
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

	ok, err := c.Repos.Inventory.AppendItem(character, item)
	if err != nil {
		return false, fmt.Errorf("append_inventory_item: AppendItem failed: %w", err)
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

	if err := c.Repos.Inventory.RemoveItem(character, name); err != nil {
		return fmt.Errorf("remove_inventory_item: %w", err)
	}

	return nil
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

	ok, err := c.Repos.Inventory.SetCurrency(character, name, count)
	if err != nil {
		return false, fmt.Errorf("set_currency: SetCurrency failed: %w", err)
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

	if err := c.Repos.Inventory.RemoveCurrency(character, name); err != nil {
		return fmt.Errorf("character.RemoveCurrency: %w", err)
	}

	return nil
}

// Append is the legacy single-method dispatch kept
// for the legacy `update_character` tool. It
// routes to the right Append* method based on
// the file argument. file ∈ {SOUL, skill, memory}.
func (c *Character) Append(character, file, section, value string) error {
	switch strings.ToLower(file) {
	case "soul":
		_, err := c.AppendSoul(character, section, value)

		return err
	case "skill":
		_, err := c.AppendSkill(character, section, value)

		return err
	case "memory":
		_, err := c.AppendMemorySection(character, section, value)

		return err
	default:
		return ErrUnknownCharacterFile
	}
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
	if s, err := c.Repos.Soul.Load(activeChar); err == nil {
		if body, err := s.Save(); err == nil {
			snap.SOUL = body
		}
	}

	if s, err := c.Repos.Skill.Load(activeChar); err == nil {
		if body, err := s.Save(); err == nil {
			snap.SKILL = body
		}
	}

	if m, err := c.Repos.Memory.Load(activeChar); err == nil {
		if body, err := m.Save(); err == nil {
			snap.Memory = body
		}
	}

	if inv, err := c.Repos.Inventory.Load(activeChar); err == nil {
		if body, err := inv.Save(); err == nil {
			snap.Inventory = body
		}
	}

	if activeWorld != "" {
		if s, err := c.Repos.WorldState.Load(activeWorld); err == nil {
			snap.Day = s.Day
			if body, rerr := renderStateBody(s); rerr == nil {
				snap.State = body
			}
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
	appendf(&b, "**Персонаж: %s**\n", s.Character)

	if s.World != "" {
		appendf(&b, "**Мир: %s** (день %d)\n\n", s.World, s.Day)
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
			appendf(&b, "## %s\n_(пусто)_\n\n", sec.title)

			continue
		}

		appendf(&b, "## %s\n", sec.title)
		b.WriteString(truncateForMe(sec.body, maxPerSection))
		b.WriteString("\n\n")
	}

	return strings.TrimSpace(b.String())
}

// appendf writes a formatted string to b and discards the
// (always-nil) error from fmt.Fprintf. strings.Builder.Write
// is the only Write call here, so the error cannot occur at
// runtime; this keeps the call sites single-line.
func appendf(b *strings.Builder, format string, args ...any) {
	_, _ = fmt.Fprintf(b, format, args...)
}

func truncateForMe(s string, maxLines int) string {
	if maxLines <= 0 {
		return s
	}

	lines := strings.Split(s, "\n")
	if len(lines) <= maxLines {
		return s
	}

	return strings.Join(lines[:maxLines], "\n") + fmt.Sprintf("\n[…+%d строк обрезано…]", len(lines)-maxLines)
}

// enumContains is duplicated from charprofile to
// avoid exporting the helper there — the file
// toolset is the only caller.
func enumContains(target string, list []string) bool {
	return slices.Contains(list, target)
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
