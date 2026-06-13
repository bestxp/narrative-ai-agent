package files

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/rs/zerolog"
	"gopkg.in/yaml.v3"

	"github.com/bestxp/narrative-ai-agent/internal/adapter/storage"
	"github.com/bestxp/narrative-ai-agent/internal/charprofile"
	"github.com/bestxp/narrative-ai-agent/internal/slowlog"
	"github.com/bestxp/narrative-ai-agent/internal/usecase/tools"
)

// Character is the file-backed implementation of
// tools.CharacterTool: read/append on the four
// per-character YAML files (SOUL/skill/memory/
// inventory), the /me snapshot, and the legacy
// markdown → YAML migration.
//
// The h5 refactor moved all character storage to
// YAML (see planning/char_format.md). The free-form
// markdown parsers (upsertSection /
// stateSectionNames / classifySection) are gone —
// YAML's data: [{section, values}] makes the
// "where does this fact go" question structural
// instead of stringy.
type Character struct {
	fs   *storage.FileStore
	log  zerolog.Logger
	slow *slowlog.Logger
	// migrator is the LLM-driven cleanup hook used
	// the first time a legacy .md is loaded. nil
	// means "deterministic fallback only" — the
	// charprofile.MigrateFromMarkdown path. Today
	// main.go does not wire a migrator; once
	// memory_summary.md and a dedicated role land
	// (see the h5 backlog), main.go will pass the
	// summarizer adapter in via NewCharacter.
	migrator Migrator
	// world returns the active world name. The
	// indirection lets tests pass a custom resolver
	// without re-constructing the toolset.
	world func() string
}

// Migrator is the LLM-driven migration hook. The
// implementation lives in cmd/bot/main.go (it
// wraps *usecase.Summarizer and reads the legacy
// file + memorise.md, then calls the summary role
// to produce clean YAML). Returning the empty
// string is treated as "no LLM help, use the
// deterministic fallback".
type Migrator interface {
	MigrateCharacterFile(ctx context.Context, kind, name, legacy, memorise string) (string, error)
}

// SetMigrator wires the LLM-driven migrator.
// nil clears the field (deterministic only).
func (c *Character) SetMigrator(m Migrator) { c.migrator = m }

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
			// Avoid an import cycle on domain by
			// reusing the parse path through the
			// World helper below — currentWorld is
			// in state.go of this package.
			return currentWorld(fs)
		},
	}
}

// Public errors. Callers can errors.Is against
// them without importing this package's internals.
var (
	ErrUnknownCharacterFile = errors.New("character: file kind must be soul, skill, memory or inventory")
	ErrUnknownInventoryOp   = errors.New("character: inventory op must be append, remove_item, set_currency or remove_currency")
	ErrEmptySection         = errors.New("character: section must not be empty")
	ErrEmptyAppend          = errors.New("character: append must not be empty")
	ErrNoActiveCharacter    = errors.New("character: no active character in registry")
)

// --- file path helpers (centralised) ---

func soulPath(charDir string) string {
	return "characters/" + charDir + "/SOUL.yaml"
}
func skillPath(charDir string) string {
	return "characters/" + charDir + "/skill.yaml"
}
func memoryPath(charDir string) string {
	return "characters/" + charDir + "/memory.yaml"
}
func inventoryPath(charDir string) string {
	return "characters/" + charDir + "/inventory.yaml"
}

// --- migration ---

// MigrateLegacy scans a character directory for
// legacy .md files (SOUL.md / SKILL.md / memory.md)
// and converts each to the new YAML format. The
// legacy .md is renamed to .bak on success. Returns
// the list of converted kinds (soul / skill /
// memory).
//
// The migration path:
//
//  1. Read the legacy .md.
//  2. If a migrator is wired, ask it to produce
//     clean YAML using memorise.md as context.
//     The migrator returns just the YAML body —
//     no markdown wrapping, no ` ```yaml ` fences.
//  3. If the migrator returns an error OR the
//     returned body is not parseable YAML, fall
//     back to charprofile.MigrateFromMarkdown
//     (deterministic section-parsing).
//  4. Write the new YAML, rename the legacy .md
//     to .bak.
//
// The function is idempotent: a missing legacy
// file is a no-op for that kind. Calling Migrate
// twice in a row is safe.
func (c *Character) MigrateLegacy(ctx context.Context, charDir, world string) ([]string, error) {
	if charDir == "" {
		return nil, ErrNoActiveCharacter
	}
	converted := []string{}
	memorise := ""
	if world != "" {
		memorise, _ = c.fs.ReadRaw("worlds/" + world + "/memorise.md")
	}
	for _, kind := range []string{"SOUL", "skill", "memory"} {
		legacyRel := legacyPath(charDir, kind)
		legacy, err := c.fs.ReadRaw(legacyRel)
		if err != nil || strings.TrimSpace(legacy) == "" {
			continue
		}
		// Already-migrated check: if the YAML file
		// exists, the operator has run the bot
		// before the refactor. Leave both alone.
		if c.fs.Exists(yamlPath(charDir, kind)) {
			continue
		}
		body, err := c.migrateOne(ctx, kind, charDir, legacy, memorise)
		if err != nil {
			c.log.Warn().Err(err).Str("kind", kind).Str("character", charDir).Msg("character migrate failed; leaving legacy in place")
			continue
		}
		// Write YAML, then rename legacy.
		if err := c.fs.WriteRawAtomic(yamlPath(charDir, kind), body); err != nil {
			c.log.Warn().Err(err).Msg("character migrate: write yaml failed")
			continue
		}
		if err := c.fs.WriteRawAtomic(legacyRel+".bak", legacy); err != nil {
			c.log.Warn().Err(err).Msg("character migrate: backup rename failed")
		}
		// Original .md path stays put. Operator
		// can delete it once the YAML looks
		// right. Renaming the .md in place is
		// unsafe because the FileStore helpers
		// keep pointing at the .md for one
		// more turn (race).
		converted = append(converted, kind)
		c.log.Info().Str("kind", kind).Str("character", charDir).Msg("character migrated to YAML")
	}
	return converted, nil
}

// migrateOne runs the LLM-driven path if a
// migrator is wired, otherwise the deterministic
// fallback. The return value is the YAML body
// (including the trailing newline, with stable
// field order — yaml.Marshal was applied).
func (c *Character) migrateOne(ctx context.Context, kind, name, legacy, memorise string) (string, error) {
	if c.migrator != nil {
		raw, err := c.migrator.MigrateCharacterFile(ctx, kind, name, legacy, memorise)
		if err == nil {
			// Verify the LLM response is
			// parseable YAML of the expected
			// shape. If it parses, normalise
			// and use it.
			if normalised, ok := normaliseMigratedYAML(kind, raw); ok {
				return normalised, nil
			}
			c.log.Warn().Str("kind", kind).Str("character", name).Msg("LLM migration: response failed YAML parse; using deterministic fallback")
		} else {
			c.log.Warn().Err(err).Str("kind", kind).Msg("LLM migration: error; using deterministic fallback")
		}
	}
	// Deterministic fallback. fileSlug is the
	// character dir — it goes into the `name`
	// field if the legacy file had no H1.
	fileSlug := name
	got, err := charprofile.MigrateFromMarkdown(kind, legacy, fileSlug)
	if err != nil {
		return "", err
	}
	switch v := got.(type) {
	case charprofile.Soul:
		out, err := v.Save()
		return out, err
	case charprofile.Skill:
		out, err := v.Save()
		return out, err
	case charprofile.Memory:
		out, err := v.Save()
		return out, err
	}
	return "", fmt.Errorf("character migrate: unknown kind %q", kind)
}

// normaliseMigratedYAML unmarshals the LLM's
// response into the expected type, re-marshals
// it (stable field order) and returns the
// canonical body. Returns ok=false if the
// response is not the expected shape.
func normaliseMigratedYAML(kind, raw string) (string, bool) {
	switch kind {
	case "SOUL":
		var s charprofile.Soul
		if err := yaml.Unmarshal([]byte(raw), &s); err != nil {
			return "", false
		}
		out, err := s.Save()
		if err != nil {
			return "", false
		}
		return out, true
	case "skill":
		var s charprofile.Skill
		if err := yaml.Unmarshal([]byte(raw), &s); err != nil {
			return "", false
		}
		out, err := s.Save()
		if err != nil {
			return "", false
		}
		return out, true
	case "memory":
		var m charprofile.Memory
		if err := yaml.Unmarshal([]byte(raw), &m); err != nil {
			return "", false
		}
		out, err := m.Save()
		if err != nil {
			return "", false
		}
		return out, true
	}
	return "", false
}

// legacyPath / yamlPath pick the on-disk path
// for a given kind.
func legacyPath(charDir, kind string) string {
	switch kind {
	case "SOUL":
		return "characters/" + charDir + "/SOUL.md"
	case "skill":
		return "characters/" + charDir + "/SKILL.md"
	case "memory":
		return "characters/" + charDir + "/memory.md"
	}
	return ""
}

func yamlPath(charDir, kind string) string {
	switch kind {
	case "SOUL":
		return soulPath(charDir)
	case "skill":
		return skillPath(charDir)
	case "memory":
		return memoryPath(charDir)
	}
	return ""
}

// --- Append (per file kind) ---

// AppendSoul adds a value to a SOUL.yaml section.
// Sections are free-form — anything the LLM
// invents is accepted. The section is auto-created
// on first write.
//
// Returns true if the file changed (a new section
// or a new value). false means the value was an
// exact-string duplicate.
func (c *Character) AppendSoul(charDir, section, value string) (bool, error) {
	if charDir == "" {
		return false, ErrNoActiveCharacter
	}
	if strings.TrimSpace(section) == "" {
		return false, ErrEmptySection
	}
	if strings.TrimSpace(value) == "" {
		return false, ErrEmptyAppend
	}
	body, _ := c.fs.ReadRaw(soulPath(charDir))
	s, err := charprofile.LoadSoul(body)
	if err != nil {
		// New file — start from a fresh seed
		// using the operator's character name
		// (best effort — the LLM can fix it on
		// the next turn).
		s = charprofile.Soul{}
		s.Name = charDir
	}
	changed := s.Append(section, value)
	if !changed {
		return false, nil
	}
	if err := c.persist(charDir, "SOUL", s, body); err != nil {
		return false, err
	}
	return true, nil
}

// AppendSkill adds a value to a skill.yaml section.
// The section MUST be on the fixed enum
// (charprofile.SkillFixedSections) — anything
// else returns ErrSectionNotFound and is a
// no-op.
func (c *Character) AppendSkill(charDir, section, value string) (bool, error) {
	if charDir == "" {
		return false, ErrNoActiveCharacter
	}
	if strings.TrimSpace(section) == "" {
		return false, ErrEmptySection
	}
	if strings.TrimSpace(value) == "" {
		return false, ErrEmptyAppend
	}
	body, _ := c.fs.ReadRaw(skillPath(charDir))
	s, err := charprofile.LoadSkill(body)
	if err != nil {
		s = charprofile.Skill{}
	}
	if !enumContains(section, charprofile.SkillFixedSections) {
		return false, charprofile.ErrSectionNotFound
	}
	changed := s.Append(section, value)
	if !changed {
		return false, nil
	}
	if err := c.persist(charDir, "skill", s, body); err != nil {
		return false, err
	}
	return true, nil
}

// AppendMemorySection adds a value to a memory.yaml
// section. Same strict-enum rule as Skill.
//
// Renamed from AppendMemory to avoid the name
// collision with the *Memory concern (the legacy
// method that appended to the per-character
// memory.md journal — that file is gone in the h5
// refactor; *Memory.AppendMemory is still used for
// the world's memorise.md).
func (c *Character) AppendMemorySection(charDir, section, value string) (bool, error) {
	if charDir == "" {
		return false, ErrNoActiveCharacter
	}
	if strings.TrimSpace(section) == "" {
		return false, ErrEmptySection
	}
	if strings.TrimSpace(value) == "" {
		return false, ErrEmptyAppend
	}
	body, _ := c.fs.ReadRaw(memoryPath(charDir))
	m, err := charprofile.LoadMemory(body)
	if err != nil {
		m = charprofile.Memory{}
	}
	if !enumContains(section, charprofile.MemoryFixedSections) {
		return false, charprofile.ErrSectionNotFound
	}
	changed := m.Append(section, value)
	if !changed {
		return false, nil
	}
	if err := c.persist(charDir, "memory", m, body); err != nil {
		return false, nil
	}
	return true, nil
}

// AppendInventoryItem adds or REPLACES an item
// in inventory.yaml. The item is identified by
// its Name field; description, equip and special
// are written verbatim. Returns true if the file
// changed.
//
// Quantity is encoded in the name itself (one
// items[] entry per unit, or "Кунай x3"). The
// model picks the form. We do not enforce
// either.
func (c *Character) AppendInventoryItem(charDir string, item charprofile.Item) (bool, error) {
	if charDir == "" {
		return false, ErrNoActiveCharacter
	}
	if strings.TrimSpace(item.Name) == "" {
		return false, ErrEmptySection
	}
	body, _ := c.fs.ReadRaw(inventoryPath(charDir))
	inv, err := charprofile.LoadInventory(body)
	if err != nil {
		inv = charprofile.Inventory{}
	}
	changed := inv.AppendItem(item)
	if !changed {
		return false, nil
	}
	if err := c.persistInventory(charDir, inv); err != nil {
		return false, err
	}
	return true, nil
}

// RemoveInventoryItem deletes an item by name.
// Returns charprofile.ErrItemNotFound if the
// item is not present — the Tool layer surfaces
// that string to the model so it can recover.
func (c *Character) RemoveInventoryItem(charDir, name string) error {
	if charDir == "" {
		return ErrNoActiveCharacter
	}
	if strings.TrimSpace(name) == "" {
		return ErrEmptySection
	}
	body, _ := c.fs.ReadRaw(inventoryPath(charDir))
	inv, err := charprofile.LoadInventory(body)
	if err != nil {
		return charprofile.ErrItemNotFound
	}
	if err := inv.RemoveItem(name); err != nil {
		return err
	}
	return c.persistInventory(charDir, inv)
}

// SetCurrency REPLACES the count of a currency
// line in inventory.yaml. Returns true if the
// line was created or updated.
func (c *Character) SetCurrency(charDir, name string, count int) (bool, error) {
	if charDir == "" {
		return false, ErrNoActiveCharacter
	}
	if strings.TrimSpace(name) == "" {
		return false, ErrEmptySection
	}
	body, _ := c.fs.ReadRaw(inventoryPath(charDir))
	inv, err := charprofile.LoadInventory(body)
	if err != nil {
		inv = charprofile.Inventory{}
	}
	changed := inv.SetCurrency(name, count)
	if !changed {
		return false, nil
	}
	if err := c.persistInventory(charDir, inv); err != nil {
		return false, err
	}
	return true, nil
}

// RemoveCurrency deletes a currency line.
func (c *Character) RemoveCurrency(charDir, name string) error {
	if charDir == "" {
		return ErrNoActiveCharacter
	}
	if strings.TrimSpace(name) == "" {
		return ErrEmptySection
	}
	body, _ := c.fs.ReadRaw(inventoryPath(charDir))
	inv, err := charprofile.LoadInventory(body)
	if err != nil {
		return charprofile.ErrItemNotFound
	}
	if err := inv.RemoveCurrency(name); err != nil {
		return err
	}
	return c.persistInventory(charDir, inv)
}

// --- per-kind Append (legacy dispatch) ---

// Append is the legacy single-method dispatch kept
// for the legacy `update_character` tool. It
// routes to the right Append* method based on
// the file argument. file ∈ {SOUL, skill, memory}.
// inventory is not reachable from here — use
// AppendInventoryItem.
//
// The section/append fields are the same as the
// old per-md tool; we just round-trip through
// charprofile instead of upsertSection.
func (c *Character) Append(charDir, file, section, value string) error {
	var (
		changed bool
		err     error
	)
	switch strings.ToLower(file) {
	case "soul":
		changed, err = c.AppendSoul(charDir, section, value)
	case "skill":
		changed, err = c.AppendSkill(charDir, section, value)
	case "memory":
		changed, err = c.AppendMemorySection(charDir, section, value)
	default:
		return ErrUnknownCharacterFile
	}
	if err != nil {
		return err
	}
	if !changed {
		// Idempotent no-op — return nil so the
		// tool result is "appended" either way
		// (we do not want the LLM to retry).
		_ = changed
	}
	return nil
}

// --- read / snapshot ---

// Read returns the snapshot of the current character.
// All four YAML files are loaded and the canonical
// body is returned in the snapshot fields. Inventory
// is added as a new field on CharacterSnapshot
// (defined in tools.go) — the /me format renders
// it under "## inventory.yaml".
func (c *Character) Read(activeChar, activeWorld string) (*tools.CharacterSnapshot, error) {
	if activeChar == "" {
		return nil, ErrNoActiveCharacter
	}
	soul, _ := c.fs.ReadRaw(soulPath(activeChar))
	skill, _ := c.fs.ReadRaw(skillPath(activeChar))
	mem, _ := c.fs.ReadRaw(memoryPath(activeChar))
	inv, _ := c.fs.ReadRaw(inventoryPath(activeChar))
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
		Inventory: inv,
		State:     state,
		Day:       day,
	}, nil
}

// --- persist ---

// persist serialises the value, writes it to the
// canonical YAML path, and emits a slowlog event
// on success. The interface is the generic
// Save()-er: Soul, Skill, Memory all qualify.
func (c *Character) persist(charDir, fileLabel string, v interface {
	Save() (string, error)
}, prevBody string) error {
	body, err := v.Save()
	if err != nil {
		return err
	}
	var rel string
	switch fileLabel {
	case "SOUL":
		rel = soulPath(charDir)
	case "skill":
		rel = skillPath(charDir)
	case "memory":
		rel = memoryPath(charDir)
	}
	if err := c.fs.WriteRawAtomic(rel, body); err != nil {
		return err
	}
	c.log.Info().
		Str("character", charDir).
		Str("file", fileLabel+".yaml").
		Int("bytes_added", len(body)-len(prevBody)).
		Msg("character_update")
	if c.slow != nil {
		_ = c.slow.Write("character.update", "", map[string]any{
			"character": charDir,
			"file":      fileLabel + ".yaml",
		})
	}
	return nil
}

// persistInventory writes the inventory and emits
// a slowlog event. Split from persist because
// Inventory's Save is in the same charprofile
// package but a different signature.
func (c *Character) persistInventory(charDir string, inv charprofile.Inventory) error {
	body, err := inv.Save()
	if err != nil {
		return err
	}
	if err := c.fs.WriteRawAtomic(inventoryPath(charDir), body); err != nil {
		return err
	}
	c.log.Info().
		Str("character", charDir).
		Str("file", "inventory.yaml").
		Msg("inventory_update")
	if c.slow != nil {
		_ = c.slow.Write("inventory.update", "", map[string]any{
			"character": charDir,
			"file":      "inventory.yaml",
		})
	}
	return nil
}

// --- /me rendering ---

// FormatSnapshot renders a snapshot for /me. Caps
// body sizes to keep the Telegram message under
// the 4096-char limit and to avoid dumping a
// multi-thousand-line lore file on a status
// check.
//
// Kept as a package-level function rather than a
// method on tools.CharacterSnapshot so the
// interface type stays free of presentation
// concerns. Callers (dispatcher.cmdMe) do
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
// a state.md body. Returns (n, true) on hit, (0,
// false) when the marker is missing — callers can
// fall back to "day 1" without surfacing an error.
//
// Duplicated from the State struct's private
// helper so each file is self-contained; the regex
// is small.
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
