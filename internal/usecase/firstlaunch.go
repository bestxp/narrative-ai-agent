package usecase

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/rs/zerolog"

	"github.com/bestxp/narrative-ai-agent/internal/adapter/storage"
	"github.com/bestxp/narrative-ai-agent/internal/charprofile"
	"github.com/bestxp/narrative-ai-agent/internal/domain"
)

// FirstLaunch creates the entire on-disk skeleton for a new player +
// world pair. It is idempotent: if info.yaml already exists and points
// at a different character, an error is returned and nothing is touched.
type FirstLaunch struct {
	fs  *storage.FileStore
	log zerolog.Logger
}

func NewFirstLaunch(fs *storage.FileStore) *FirstLaunch {
	return NewFirstLaunchWithLogger(fs, zerolog.Nop())
}

func NewFirstLaunchWithLogger(fs *storage.FileStore, log zerolog.Logger) *FirstLaunch {
	return &FirstLaunch{fs: fs, log: log.With().Str("component", "first_launch").Logger()}
}

type CharacterSpec struct {
	DisplayName string // human-readable
	Dir         string // latin, sanitised
	TrueNature  string
	Philosophy  string
}

type WorldSpec struct {
	DisplayName string
	Dir         string
	IsKnown     bool
	Canon       string // для известного — справочник, для выдуманного — сценарий
}

var (
	ErrAlreadyLaunched = errors.New("first launch: game-data/info.yaml already exists")
	ErrInvalidSpec     = errors.New("first launch: invalid character or world name")
)

func (f *FirstLaunch) Launch(char CharacterSpec, world WorldSpec) error {
	if f.fs.Exists(storage.InfoFile) {
		return ErrAlreadyLaunched
	}
	charDir, err := domain.SanitizeName(char.Dir)
	if err != nil {
		return fmt.Errorf("character dir: %w", err)
	}
	worldDir, err := domain.SanitizeName(world.Dir)
	if err != nil {
		return fmt.Errorf("world dir: %w", err)
	}
	if err := f.writeCharacter(charDir, char); err != nil {
		return err
	}
	if err := f.writeWorld(worldDir, world); err != nil {
		return err
	}
	if err := f.fs.WriteRawAtomic(storage.InfoFile, domain.BuildInfo(charDir, worldDir, nil, nil)); err != nil {
		return err
	}
	f.log.Info().Str("character", charDir).Str("world", worldDir).Msg("first_launch")
	return nil
}

// writeCharacter seeds the four YAML files for a
// new character:
//
//	SOUL.yaml      — who the GG is
//	skill.yaml     — what the GG can do (fixed enum)
//	memory.yaml    — what the GG remembers (fixed enum)
//	inventory.yaml — what the GG has on them
//
// Legacy .md files are detected on first launch and
// migrated through the LLM-driven path (with
// charprofile.MigrateFromMarkdown as fallback).
// Today the firstlaunch seed is plain YAML; the
// migration is wired in tools/files/character.go.
func (f *FirstLaunch) writeCharacter(dir string, c CharacterSpec) error {
	root := "characters/" + dir
	if err := f.fs.EnsureDir(root); err != nil {
		return err
	}
	if err := f.fs.WriteRawAtomic(root+"/SOUL.yaml", buildSeedSoul(c)); err != nil {
		return err
	}
	if err := f.fs.WriteRawAtomic(root+"/skill.yaml", buildSeedSkill(c)); err != nil {
		return err
	}
	if err := f.fs.WriteRawAtomic(root+"/memory.yaml", buildSeedMemory(c)); err != nil {
		return err
	}
	return f.fs.WriteRawAtomic(root+"/inventory.yaml", buildSeedInventory(c))
}

// buildSeedSoul renders the canonical SOUL.yaml
// seed. The "soul" line is a one-sentence summary;
// the body is a single "Истинная сущность" value
// derived from the operator's TrueNature. Other
// sections (Предпочтения / Философия и принципы /
// Прочее) start empty and the LLM fills them as
// the character develops.
func buildSeedSoul(c CharacterSpec) string {
	s := charprofile.Soul{
		Soul: strings.TrimSpace(c.TrueNature),
	}
	s.Name = strings.TrimSpace(c.DisplayName)
	if s.Soul == "" {
		s.Soul = "—"
	}
	if strings.TrimSpace(c.Philosophy) != "" {
		s.Data = []charprofile.Section{
			{Name: "Истинная сущность", Values: []string{strings.TrimSpace(c.TrueNature)}},
			{Name: "Философия и принципы", Values: []string{strings.TrimSpace(c.Philosophy)}},
		}
	} else if strings.TrimSpace(c.TrueNature) != "" {
		s.Data = []charprofile.Section{
			{Name: "Истинная сущность", Values: []string{strings.TrimSpace(c.TrueNature)}},
		}
	}
	out, _ := s.Save()
	return out
}

// buildSeedSkill renders the canonical skill.yaml
// seed: every fixed-enum section present and empty,
// so the LLM has the full template to append into.
// No values are pre-filled — the operator's spec
// does not include "weapons at launch" today, and
// seeding sample values would just be noise the
// LLM would later have to delete.
//
// The character name is NOT seeded here. SOUL.yaml
// is the canonical place for the display name; the
// other three files (skill / memory / inventory)
// derive their identity from the directory.
func buildSeedSkill(c CharacterSpec) string {
	s := charprofile.Skill{}
	for _, name := range charprofile.SkillFixedSections {
		s.Data = append(s.Data, charprofile.Section{Name: name})
	}
	out, _ := s.Save()
	return out
}

// buildSeedMemory renders the canonical memory.yaml
// seed: every fixed-enum section present and
// empty. The header comment in the markdown era
// ("Субъективные моменты. От первого лица.")
// does not survive in YAML — the file's name and
// section headers are self-describing.
//
// The character name is NOT seeded here.
func buildSeedMemory(c CharacterSpec) string {
	m := charprofile.Memory{}
	for _, name := range charprofile.MemoryFixedSections {
		m.Data = append(m.Data, charprofile.Section{Name: name})
	}
	out, _ := m.Save()
	return out
}

// buildSeedInventory renders the canonical
// inventory.yaml seed: an empty file with empty
// currency and items arrays. The model will
// start adding items on the first scene.
func buildSeedInventory(c CharacterSpec) string {
	inv := charprofile.Inventory{}
	out, _ := inv.Save()
	return out
}

func (f *FirstLaunch) writeWorld(dir string, w WorldSpec) error {
	root := "worlds/" + dir
	if err := f.fs.EnsureDir(root + "/characters"); err != nil {
		return err
	}
	canon := "# " + strings.TrimSpace(w.DisplayName) + " — канон/сценарий\n" + strings.TrimSpace(w.Canon) + "\n"
	if err := f.fs.WriteRawAtomic(root+"/canon.md", canon); err != nil {
		return err
	}
	state := StateHeader(1, true) + "\nСтартовая сцена.\n"
	if err := f.fs.WriteRawAtomic(root+"/state.md", state); err != nil {
		return err
	}
	lore := "# Мир " + strings.TrimSpace(w.DisplayName) + "\nКанон актуален, если игрок не вносит изменения.\n"
	if err := f.fs.WriteRawAtomic(root+"/lore.md", lore); err != nil {
		return err
	}
	if err := f.fs.WriteRawAtomic(root+"/plan.md", defaultPlan(dir)); err != nil {
		return err
	}
	if err := f.fs.WriteRawAtomic(root+"/chronicle.yaml", "days: {}\nperiods: []\n"); err != nil {
		return err
	}
	if err := f.fs.WriteRawAtomic(root+"/staging.yaml", defaultStaging(dir)); err != nil {
		return err
	}
	// The NPC registry (characters.yaml) is NOT seeded
	// here. It is created lazily on the first
	// create_npc call via the worldregistry package —
	// earlier revisions wrote an empty characters.md
	// table on first launch which produced
	// duplicate-NPC cases where one registry listed a
	// character that the other did not.
	return nil
}

// defaultStaging returns the canonical sandbox staging.yaml
// for a brand-new world: enabled=false, empty graph. The
// operator edits this file to switch the world into a
// staged story arc; until then the staging system is silent
// and the WorldState user message omits the stage block.
func defaultStaging(dir string) string {
	var b strings.Builder
	b.WriteString("# Staging for world: ")
	b.WriteString(dir)
	b.WriteString("\n# Set `enabled: true` and fill `init` + `stages` to switch this world to a staged story arc.\n")
	b.WriteString("enabled: false\n")
	b.WriteString("init: []\n")
	b.WriteString("stages: []\n")
	return b.String()
}

// defaultPlan returns the canonical 3-event starter plan for
// a brand-new world. Equivalent to
// Maintenance.RotatePlan(dir, [...]) but inlined here so
// firstlaunch does not have to spin up a full toolset just
// to write four lines.
func defaultPlan(dir string) string {
	var b strings.Builder
	b.WriteString("# План: " + dir + "\n\n")
	for i, e := range []string{
		"вводная сцена: знакомство с миром",
		"первая зацепка / конфликт",
		"первая развилка",
	} {
		b.WriteString("- День +")
		b.WriteString(strconv.Itoa(i + 1))
		b.WriteString(": ")
		b.WriteString(e)
		b.WriteString("\n")
	}
	return b.String()
}
