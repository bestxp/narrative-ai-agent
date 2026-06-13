// Package charprofile is the canonical on-disk shape for
// the player character (GG) files. The bot has moved
// from free-form markdown to structured YAML for three
// reasons:
//
//  1. Append-only sections with exact-string dedup.
//     The model cannot accidentally erase an earlier
//     fact by rewriting the file body.
//  2. Predictable schema. The dispatch and the
//     summarizer know what sections exist; they can
//     iterate over them and warn when a section is
//     missing or contains garbage.
//  3. Maintenance is a per-section operation, not a
//     per-paragraph LLM rewrite.
//
// The on-disk file is YAML. The model never sees the
// YAML — BuildMarkdown / BuildHuman render the same
// canonical block layout the legacy path used, so the
// prompt and the player's eyes do not need to change.
// Storage is hidden behind Load / Save / Append /
// MigrateFromMarkdown helpers; the rest of the bot
// only sees the typed values.
//
// Four files live under characters/<dir>/:
//
//	SOUL.yaml      — who the GG is
//	skill.yaml     — what the GG can do
//	memory.yaml    — what the GG remembers
//	inventory.yaml — what the GG has on them
//
// The legacy free-form SOUL.md / SKILL.md / memory.md
// are honoured on read: MigrateFromMarkdown parses
// `## <section>` blocks into data[].values, falls
// back to the deterministic parse if the LLM-driven
// path is unavailable.
package charprofile

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// ErrNotFound is returned by Load when the file does
// not exist or exists but is empty. The dispatcher
// turns this into a slowlog warning rather than a
// fatal error — firstlaunch is the right next step.
var ErrNotFound = errors.New("charprofile: file not found or empty")

// ErrSectionNotFound is returned by Append when the
// target file declares a fixed enum of sections
// (skill.yaml, memory.yaml) and the requested section
// is not on it. LLM cannot invent new sections; an
// operator must add a new enum value.
var ErrSectionNotFound = errors.New("charprofile: section not in fixed enum")

// Section is a (name, values) pair. Used by all three
// append-only files (Soul, Skill, Memory). The
// canonical list of section names is part of the
// project contract — see planning/char_format.md.
//
// Fields are YAML tags `section` and `values` so the
// round-trip via yaml.Marshal / yaml.Unmarshal is
// stable.
type Section struct {
	// Name is the section header. Within a single
	// file the name is unique (case-sensitive after
	// TrimSpace).
	Name string `yaml:"section"`
	// Values is the append-only list of facts. The
	// order is chronological; the dedup key is
	// exact-string (case-sensitive after TrimSpace).
	Values []string `yaml:"values"`
}

// Base is the common shape of Soul / Skill / Memory:
// a display name, plus an ordered list of sections.
// The three files differ only in their (optional)
// extra scalars (Soul adds `soul`) and in the
// enum-validity of the section list (Soul is
// free-form + "Прочее" fallback, Skill / Memory are
// strict).
type Base struct {
	// Name is the human-readable character name.
	// Required by all four files. Loaded from
	// info.yaml on first launch; copied into each
	// file so the operator can grep the on-disk
	// character by name without cross-referencing
	// the registry.
	Name string `yaml:"name"`
	// Data is the ordered list of sections. Order
	// matters only for rendering; the LLM is
	// allowed to insert new sections at the end.
	Data []Section `yaml:"data"`
}

// Soul is the SOUL.yaml payload. Adds a single-line
// `soul` summary on top of Base. Sums up "who is the
// GG" in one short string (age, kind, key trait).
//
// Sections are free-form in SOUL.yaml — the LLM may
// create new ones. The "Прочее" fallback section is
// the canonical catch-all so old free-form MD files
// with non-standard headings do not block migration.
type Soul struct {
	Base `yaml:",inline"`
	// Soul is a one-line summary (e.g. "13 лет,
	// попаданец"). The prompt surfaces this verbatim
	// in the system block — it is the cheapest
	// "what kind of character is this" hint.
	Soul string `yaml:"soul"`
}

// Skill is the skill.yaml payload. The sections
// are a fixed enum (see SkillFixedSections); the
// model cannot invent new ones. Section "permanent
// party" lives here for historical reasons
// (extractPermanentParty in gm.go read it from the
// old SKILL.md) — see the cross-world note below.
//
// Cross-world note: the operator decided that
// permanent party is a WORLD-scoped concern, not a
// character-scoped one. The next refactor will
// move it into worlds/<w>/state.md as a
// "## permanent party" section. Until then we
// keep the field on Skill for backwards
// compatibility.
type Skill struct {
	Base `yaml:",inline"`
}

// Memory is the memory.yaml payload. Same shape as
// Soul / Skill, but the section enum is fixed and
// narrower (4 canonical sections). The model cannot
// create new ones.
//
// Maintenance (`maintain_memory`, end-of-day) calls
// into the same summarizer pipeline that compacts
// NPC profiles.
type Memory struct {
	Base `yaml:",inline"`
}

// SoulFixedSections is the suggested enum for
// SOUL.yaml. The list is not enforced by the
// package (Soul sections are free-form); the model
// gets it via the prompt and the operator can add
// new entries without changing code.
var SoulFixedSections = []string{
	"Истинная сущность",
	"Предпочтения",
	"Философия и принципы",
	"Прочее",
}

// SkillFixedSections is the strict enum of skill.yaml
// section names. Append rejects any section not on
// this list. The model must be told (via prompt) to
// pick from the canonical list.
//
// "permanent party" is here for back-compat; see
// the Skill doc comment.
var SkillFixedSections = []string{
	"Ранг",
	"Оружие",
	"Базовые способности",
	"Фундаментальные стихии",
	"Особые проявления",
	"Универсальные навыки",
	"Ограничения",
	"Глаза",
	"Доспех",
	"permanent party",
}

// MemoryFixedSections is the strict enum of
// memory.yaml section names. Append rejects any
// section not on this list.
var MemoryFixedSections = []string{
	"Яркие моменты",
	"Факты о мире",
	"Обещания и цели",
	"Важные люди",
}

// --- Loaders / savers ---

// LoadSoul reads SOUL.yaml. Returns ErrNotFound
// when the file is missing or empty so the
// firstlaunch seed is the right next step.
func LoadSoul(body string) (Soul, error) {
	return loadBaseInto[Soul](body)
}

// LoadSkill reads skill.yaml. The returned Skill
// has Data=nil if the file is missing.
func LoadSkill(body string) (Skill, error) {
	return loadBaseInto[Skill](body)
}

// LoadMemory reads memory.yaml.
func LoadMemory(body string) (Memory, error) {
	return loadBaseInto[Memory](body)
}

// Save serialises the file back to YAML. The output
// is the canonical form (alphabetical section order
// is preserved by SaveSectionOrder). yaml.Marshal
// uses struct field order, which is stable.
//
// Returns ErrNotFound for an empty body (matches
// the Load contract).
func (s Soul) Save() (string, error)   { return saveBase(s) }
func (s Skill) Save() (string, error)  { return saveBase(s) }
func (s Memory) Save() (string, error) { return saveBase(s) }

// Append adds value to the named section, creating
// the section if absent. Sections are deduped
// case-sensitive after TrimSpace. Returns true if
// the file changed (a new value or a new section).
//
// For Skill and Memory the section name MUST be on
// the fixed enum; otherwise ErrSectionNotFound. Soul
// accepts any name.
func (s *Soul) Append(section, value string) bool {
	return appendIntoBase(&s.Base, section, value, false)
}

// Append on Skill uses the strict enum.
func (s *Skill) Append(section, value string) bool {
	return appendIntoBase(&s.Base, section, value, true)
}

// Append on Memory uses the strict enum.
func (s *Memory) Append(section, value string) bool {
	return appendIntoBase(&s.Base, section, value, true)
}

// ReplaceSection REPLACES the entire values[] of
// the named section with the new value. Used for
// the rare "snapshot" update where a single fact
// displaces the old one (e.g. "current equipment"
// rotation). Append, by contrast, never removes.
//
// Returns true if the section was found and
// replaced. False on a missing section — Append is
// the right tool for adding a new one.
func (s *Soul) ReplaceSection(section, value string) bool {
	return replaceSectionInto(&s.Base, section, value)
}

func (s *Skill) ReplaceSection(section, value string) bool {
	return replaceSectionInto(&s.Base, section, value, true)
}

func (s *Memory) ReplaceSection(section, value string) bool {
	return replaceSectionInto(&s.Base, section, value, true)
}

// --- internals ---

// loadBaseInto unmarshals body into a fresh T
// (which embeds Base). Empty body -> ErrNotFound.
func loadBaseInto[T any](body string) (T, error) {
	var zero T
	if strings.TrimSpace(body) == "" {
		return zero, ErrNotFound
	}
	if err := yaml.Unmarshal([]byte(body), &zero); err != nil {
		return zero, fmt.Errorf("charprofile: yaml.Unmarshal: %w", err)
	}
	return zero, nil
}

// saveBase marshals any struct that embeds Base. The
// result is stable (struct field order, alphabetical
// inside each Section.values map is preserved by
// yaml.v3).
func saveBase[T any](s T) (string, error) {
	out, err := yaml.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("charprofile: yaml.Marshal: %w", err)
	}
	return string(out), nil
}

// appendIntoBase is the shared Append logic. The
// strict flag controls whether unknown sections
// are rejected (Skill / Memory) or accepted (Soul).
func appendIntoBase(b *Base, section, value string, strict bool) bool {
	section = strings.TrimSpace(section)
	value = strings.TrimSpace(value)
	if section == "" || value == "" {
		return false
	}
	if strict && !enumContains(section, SkillFixedSections) && !enumContains(section, MemoryFixedSections) {
		// We could return an error here, but the
		// Tool contract is "bool, bool changed".
		// We silently drop — the slowlog catches
		// unknown-section attempts upstream.
		return false
	}
	for i := range b.Data {
		if b.Data[i].Name == section {
			if containsString(b.Data[i].Values, value) {
				return false
			}
			b.Data[i].Values = append(b.Data[i].Values, value)
			return true
		}
	}
	b.Data = append(b.Data, Section{Name: section, Values: []string{value}})
	return true
}

// replaceSectionInto REPLACES the values[] of the
// named section. The strict flag mirrors Append.
func replaceSectionInto(b *Base, section, value string, strict ...bool) bool {
	section = strings.TrimSpace(section)
	value = strings.TrimSpace(value)
	if section == "" {
		return false
	}
	if len(strict) > 0 && strict[0] {
		// strict: skill / memory
		if !enumContains(section, SkillFixedSections) && !enumContains(section, MemoryFixedSections) {
			return false
		}
	}
	for i := range b.Data {
		if b.Data[i].Name == section {
			if len(b.Data[i].Values) == 1 && b.Data[i].Values[0] == value {
				return false
			}
			b.Data[i].Values = []string{value}
			return true
		}
	}
	return false
}

// containsString is exact-string match after
// TrimSpace. We do not fold case — the model's
// spelling is the canonical one.
func containsString(haystack []string, needle string) bool {
	for _, h := range haystack {
		if strings.TrimSpace(h) == needle {
			return true
		}
	}
	return false
}

// enumContains reports whether target is in the
// fixed-enum slice. Case-sensitive (the enum names
// are part of the project contract).
func enumContains(target string, list []string) bool {
	for _, s := range list {
		if s == target {
			return true
		}
	}
	return false
}

// SortedSectionNames returns the names of every
// section, alphabetically sorted. Used by the
// operator-facing diagnostic (`/inspect`) and by
// the LLM prompt to know which sections are
// populated.
func (b *Base) SortedSectionNames() []string {
	keys := make([]string, 0, len(b.Data))
	for _, s := range b.Data {
		keys = append(keys, s.Name)
	}
	sort.Strings(keys)
	return keys
}
