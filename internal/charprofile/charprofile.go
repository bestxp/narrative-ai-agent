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
	"slices"
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

// Base is the common shape of Skill / Memory:
// an ordered list of sections. The two files
// differ only in the enum-validity of the section
// list (Skill / Memory are strict). Soul has its
// own `name` + `soul` fields (defined on the Soul
// struct) and does NOT use Base.
//
// The character name lives ONLY in SOUL.yaml.
// Duplicating it in skill.yaml / memory.yaml /
// inventory.yaml is redundant — info.yaml is the
// canonical source for the character dir, and
// SOUL.yaml is the canonical source for the
// display name. Every other file references
// SOUL.yaml implicitly through the FileStore.
type Base struct {
	// Data is the ordered list of sections. Order
	// matters only for rendering; the LLM is
	// allowed to insert new sections at the end.
	Data []Section `yaml:"data"`
}

// Soul is the SOUL.yaml payload. The ONLY file
// carrying the character name (`name`) and the
// one-line summary (`soul`). Sums up "who is the
// GG" in one short string (age, kind, key trait).
//
// Sections are free-form in SOUL.yaml — the LLM may
// create new ones. The "Прочее" fallback section is
// the canonical catch-all so old free-form MD files
// with non-standard headings do not block migration.
type Soul struct {
	// Name is the human-readable character name.
	// "Маркус Мрачный" — what shows up in the
	// system block and the narrative. Required
	// (info.yaml's `display_name` is the seed on
	// firstlaunch; on subsequent loads SOUL.yaml
	// is canonical).
	Name string `yaml:"name"`
	// Soul is a one-line summary (e.g. "13 лет,
	// попаданец"). The prompt surfaces this verbatim
	// in the system block — it is the cheapest
	// "what kind of character is this" hint.
	Soul string `yaml:"soul"`
	// Data is the ordered list of sections. Same
	// shape as Base.Data but inlined here so the
	// file renders as `name:`, `soul:`, `data:`
	// without a wrapping `base:` key.
	Data []Section `yaml:"data"`
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

// SkillFixedSections is the strict enum of skill.yaml
// section names. Append rejects any section not on
// this list. The model must be told (via prompt) to
// pick from the canonical list.
//
// "permanent party" is here for back-compat; see
// the Skill doc comment.
//
//nolint:gochecknoglobals // canonical fixed-section catalogue read by prompt + parser
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
//
//nolint:gochecknoglobals // canonical fixed-section catalogue read by prompt + parser
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
func (s *Soul) Save() (string, error)   { return saveBase(s) }
func (s *Skill) Save() (string, error)  { return saveBase(s) }
func (s *Memory) Save() (string, error) { return saveBase(s) }

// Append adds value to the named section, creating
// the section if absent. Sections are deduped
// case-sensitive after TrimSpace. Returns true if
// the file changed (a new value or a new section).
//
// For Skill and Memory the section name MUST be on
// the fixed enum; otherwise ErrSectionNotFound. Soul
// accepts any name.
func (s *Soul) Append(section, value string) bool {
	return appendIntoSections(&s.Data, section, value, false)
}

// Append on Skill uses the strict enum.
func (s *Skill) Append(section, value string) bool {
	return appendIntoSections(&s.Data, section, value, true)
}

// Append on Memory uses the strict enum.
func (s *Memory) Append(section, value string) bool {
	return appendIntoSections(&s.Data, section, value, true)
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
	return replaceSectionInto(&s.Data, section, value, false)
}

func (s *Skill) ReplaceSection(section, value string) bool {
	return replaceSectionInto(&s.Data, section, value, true)
}

func (s *Memory) ReplaceSection(section, value string) bool {
	return replaceSectionInto(&s.Data, section, value, true)
}

// --- internals ---

// loadBaseInto unmarshals body into a fresh T
// (which embeds Base). Empty body -> ErrNotFound.
//
//nolint:ireturn // generic factory returns T by design; ireturn cannot model generics.
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

// appendIntoSections is the shared Append logic. The
// strict flag controls whether unknown sections are
// rejected (Skill / Memory) or accepted (Soul).
func appendIntoSections(data *[]Section, section, value string, strict bool) bool {
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

	for i := range *data {
		if (*data)[i].Name == section {
			if containsString((*data)[i].Values, value) {
				return false
			}

			(*data)[i].Values = append((*data)[i].Values, value)

			return true
		}
	}

	*data = append(*data, Section{Name: section, Values: []string{value}})

	return true
}

// replaceSectionInto REPLACES the values[] of the
// named section. The strict flag mirrors Append.
func replaceSectionInto(data *[]Section, section, value string, strict ...bool) bool {
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

	for i := range *data {
		if (*data)[i].Name == section {
			if len((*data)[i].Values) == 1 && (*data)[i].Values[0] == value {
				return false
			}

			(*data)[i].Values = []string{value}

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
	return slices.Contains(list, target)
}

// SortedSectionNames returns the names of every
// section, alphabetically sorted. Used by the
// operator-facing diagnostic (`/inspect`) and by
// the LLM prompt to know which sections are
// populated. Defined on each file type since they
// no longer share a common Base receiver.
func (s *Soul) SortedSectionNames() []string {
	keys := make([]string, 0, len(s.Data))
	for _, sec := range s.Data {
		keys = append(keys, sec.Name)
	}

	sort.Strings(keys)

	return keys
}

func (s *Skill) SortedSectionNames() []string {
	keys := make([]string, 0, len(s.Data))
	for _, sec := range s.Data {
		keys = append(keys, sec.Name)
	}

	sort.Strings(keys)

	return keys
}

func (s *Memory) SortedSectionNames() []string {
	keys := make([]string, 0, len(s.Data))
	for _, sec := range s.Data {
		keys = append(keys, sec.Name)
	}

	sort.Strings(keys)

	return keys
}
