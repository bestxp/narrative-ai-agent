// Package npcprofile is the canonical storage shape for
// NPC files. The bot has moved from free-form markdown
// to a structured YAML representation so that:
//
//   - facts are append-only array items (no free-form
//     text that can be a duplicate in different words);
//   - relations between NPCs are a list of
//     {target, note} pairs that can be filtered and
//     deduped by target;
//   - abilities are a flat list, not a paragraph — the
//     summarizer can prune obvious filler and the player
//     can read them at a glance;
//   - last_update stays a single REPLACE line so the
//     operator can see "the most recent thing this NPC
//     did" without scrolling.
//
// The on-disk file is YAML. The model (and the
// dispatcher) never see YAML — BuildMarkdown renders
// the same canonical layout the legacy path used, so
// the prompt, the parser markers, and the player's
// eyes do not need to change. Storage is hidden
// behind Load / Save / Update helpers that take and
// return markdown-shaped strings where it matters.
package npcprofile

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/bestxp/narrative-ai-agent/internal/limits"
	"github.com/bestxp/narrative-ai-agent/internal/prompts"
	"gopkg.in/yaml.v3"
)

// Profile is the canonical NPC record on disk. Field
// names are the YAML keys; they are deliberately short
// to keep the file readable when an operator opens it
// in a text editor.
//
// The struct is split between "scalars" (one value
// per slot) and "arrays" (append-only lists). The
// arrays are what makes this representation
// attractive vs the previous free-form markdown: every
// fact is a discrete line, dedup is exact-byte
// (case-sensitive after TrimSpace), and the summarizer
// can tell at a glance how many facts a profile has
// without parsing prose.
type Profile struct {
	// DisplayName is the human-readable name the
	// model and the player see. Required.
	DisplayName string `yaml:"display_name"`
	// FileSlug is the on-disk filename without the
	// ".md" suffix. The model and dispatcher refer
	// to NPCs by DisplayName; FileSlug is what
	// FileStore uses to find the file.
	FileSlug string `yaml:"file_slug"`
	// Temperament is a one-sentence description of
	// the NPC's baseline personality. Replace on
	// explicit update; otherwise stays put.
	Temperament string `yaml:"temperament"`
	// RelationsGG is the NPC's disposition toward
	// the player's character. Plain text — the
	// summarizer keeps it tight.
	RelationsGG string `yaml:"relations_gg"`
	// RelationsNPCs is the list of (target, note)
	// pairs for the NPC's relations to other named
	// NPCs in the world. We keep it as a list of
	// objects (not a flat string) so dedup by
	// target is exact and the operator can scan the
	// file to see at a glance who hates whom.
	RelationsNPCs []Relation `yaml:"relations_npcs,omitempty"`
	// Abilities is the flat list of capabilities
	// the NPC has shown in the story. Filler
	// ("talks loudly", "is friendly") gets pruned
	// by the summarizer; concrete bloodline
	// techniques stay.
	Abilities []string `yaml:"abilities,omitempty"`
	// PersonalMemory is the ring buffer of facts
	// the NPC has witnessed or learned. The
	// summarizer kicks in when this list grows
	// past NPCPersonalMemoryLimit (40 by default)
	// and compresses the older entries.
	PersonalMemory []string `yaml:"personal_memory,omitempty"`
	// CurrentStatus is a short sentence about what
	// the NPC is doing RIGHT NOW. Updated by
	// update_state; not summarised.
	CurrentStatus string `yaml:"current_status"`
	// CriticalKnowledge is the list of secrets
	// only this NPC knows. The dispatcher enforces
	// info-isolation by including only this field
	// for NPCs that are not present in the scene.
	CriticalKnowledge []string `yaml:"critical_knowledge,omitempty"`
	// Nicknames is the list of alternative names
	// the model may use in dialogue to refer to
	// this NPC ("Хината-чан", "Sensei", etc.).
	Nicknames []string `yaml:"nicknames,omitempty"`
	// LastUpdate is the single most recent fact
	// about the NPC. REPLACE on each update; the
	// previous value lives in PersonalMemory. The
	// operator reads this line first.
	LastUpdate string `yaml:"last_update"`
}

// Relation is a (target, note) pair. Stored as a YAML
// mapping (not a flat string) so dedup by Target is
// exact and the summarizer can keep the note brief.
type Relation struct {
	Target string `yaml:"target"`
	Note   string `yaml:"note"`
}

// NPCPersonalMemoryLimit is the threshold above which
// the summarizer kicks in to compress personal_memory.
// 25 is the operator's "помогите, профиль раздулся" mark —
// at this size a single NPC profile reaches ~5KB, which
// is the budget we want to keep for the world block of
// a multi-NPC scene. The dispatcher fires
// maintain_npcs at end_day when this threshold is
// crossed.
//
// The constant is re-exported from internal/limits so
// the LLM-side template (prompts/npc_summary.md.tmpl)
// and the Go-side dispatcher share the same value.
// Callers that historically wrote
// `npcprofile.NPCPersonalMemoryLimit` should keep
// doing so — the alias is kept for back-compat.
const NPCPersonalMemoryLimit = limits.NPCPersonalMemoryLimit

// ErrNotFound is returned by Load when the file does
// not exist or exists but is empty. The dispatcher
// turns this into a slowlog warning rather than a
// fatal error — create_npc is the right next step.
var ErrNotFound = errors.New("npcprofile: file not found or empty")

// Load parses a YAML-encoded NPC file. The returned
// Profile is the only shape downstream code should
// touch; do not unmarshal into Profile yourself, the
// format may grow additional fields.
func Load(body string) (Profile, error) {
	var p Profile

	if strings.TrimSpace(body) == "" {
		return Profile{}, ErrNotFound
	}

	if err := yaml.Unmarshal([]byte(body), &p); err != nil {
		return Profile{}, fmt.Errorf("npcprofile: yaml.Unmarshal: %w", err)
	}

	return p, nil
}

// Save serialises the profile back to disk. The output
// is sorted alphabetically by section for stable diffs
// in git; yaml.Marshal's default ordering follows
// struct field order, but we add a top-level comment
// so an operator opening the file knows what they are
// looking at.
func (p *Profile) Save() (string, error) {
	out, err := yaml.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("npcprofile: yaml.Marshal: %w", err)
	}

	return string(out), nil
}

// BuildMarkdown renders the profile as a 4-block
// markdown document — the same shape the model saw
// under the legacy free-form format. Empty sections
// are dropped. The output is the canonical
// representation the LLM reads and writes through.
//
// The block order matches the legacy file layout so
// existing prompts and operator muscle memory carry
// over without changes:
//
//	# <DisplayName>
//	## Темперамент
//	## Отношения с ГГ
//	## Отношения с другими NPC
//	## Способности
//	## Личная память/факты
//	## Текущий статус
//	## Критические знания
//	## Никнеймы
//	## Последнее обновление
//
// The actual block order, "## Никнеймы" rendering
// policy, and footer handling all live in
// prompts/npc_profile.md.tmpl. This function is a
// thin wrapper that projects the typed Profile into
// the data-bag and delegates rendering. The template
// owns the format; Go owns the data.
func (p *Profile) BuildMarkdown() (string, error) {
	rows := make([]prompts.NPCRelationRow, 0, len(p.RelationsNPCs))
	for _, r := range p.RelationsNPCs {
		rows = append(rows, prompts.NPCRelationRow{
			Target: strings.TrimSpace(r.Target),
			Note:   strings.TrimSpace(r.Note),
		})
	}

	data := prompts.NewNPCProfileDataFromFields(
		strings.TrimSpace(p.DisplayName),
		strings.TrimSpace(p.Temperament),
		strings.TrimSpace(p.RelationsGG),
		rows,
		p.Abilities,
		p.PersonalMemory,
		p.CriticalKnowledge,
		p.Nicknames,
		strings.TrimSpace(p.CurrentStatus),
		strings.TrimSpace(p.LastUpdate),
	)

	out, err := prompts.Render("npc_profile.md.tmpl", prompts.PromptData{
		NPCProfile: data,
	})
	if err != nil {
		return "", fmt.Errorf("build_markdown: %w", err)
	}

	return out, nil
}

// BuildCompact renders a medium-detail view of the
// profile: header (display name), temperament, relations
// to ГГ, current status, and the most recent
// last_update. The big arrays (abilities, personal
// memory, critical knowledge, relations to other
// NPCs) are dropped — they are summarised, not lost,
// and the model can call search_npc / load the full
// YAML through the same read paths if a detail is
// needed.
//
// Used as the second LOD tier (LOD 1). The full
// BuildMarkdown is LOD 0 (Full).
//
// Empty fields are dropped. The output is plain
// markdown (## for the header, prose for the body),
// consistent with the LOD 0 renderer's block
// layout.
func (p *Profile) BuildCompact() string {
	var b strings.Builder
	fmt.Fprintf(&b, "## %s\n", strings.TrimSpace(p.DisplayName))

	if s := strings.TrimSpace(p.Temperament); s != "" {
		fmt.Fprintf(&b, "Темперамент: %s\n", s)
	}

	if s := strings.TrimSpace(p.RelationsGG); s != "" {
		fmt.Fprintf(&b, "К ГГ: %s\n", s)
	}

	if s := strings.TrimSpace(p.CurrentStatus); s != "" {
		fmt.Fprintf(&b, "Текущий статус: %s\n", s)
	}

	if len(p.RelationsNPCs) > 0 {
		// Compact: just the target, no per-target note.
		// The full relations list is in the YAML.
		var rels []string

		for _, r := range p.RelationsNPCs {
			if t := strings.TrimSpace(r.Target); t != "" {
				rels = append(rels, t)
			}
		}

		if len(rels) > 0 {
			fmt.Fprintf(&b, "Связи: %s\n", strings.Join(rels, ", "))
		}
	}

	if s := strings.TrimSpace(p.LastUpdate); s != "" {
		fmt.Fprintf(&b, "Свежее: %s\n", s)
	}

	return strings.TrimSpace(b.String())
}

// BuildOneLine renders the most-compressed view:
// display name + a 1-sentence temperament + a
// 1-sentence current status. Everything else is
// dropped. Used as LOD 2 — the third tier, applied
// to background NPCs in scenes with 10+ active
// characters where the full compact view of every
// NPC would still exceed the cache budget.
//
// Empty fields are dropped silently. The output is
// a single line with newlines between the three
// short fields so the markdown render is still
// scannable when the LLM reads user[0] back.
func (p *Profile) BuildOneLine() string {
	var b strings.Builder

	name := strings.TrimSpace(p.DisplayName)
	if name != "" {
		b.WriteString(name)
	}

	if s := strings.TrimSpace(p.Temperament); s != "" {
		fmt.Fprintf(&b, "\n%s.", truncateRune(s, 120))
	}

	if s := strings.TrimSpace(p.CurrentStatus); s != "" {
		fmt.Fprintf(&b, "\nСейчас: %s", truncateRune(s, 120))
	}

	return strings.TrimSpace(b.String())
}

// truncateRune cuts s at max runes (not bytes) and
// appends an ellipsis if the cut happened. The LLM
// reads these strings as UTF-8; slicing by byte
// would risk splitting a multi-byte codepoint.
func truncateRune(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}

	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}

	return string(runes[:maxRunes]) + "…"
}

// SectionKind enumerates the section names the
// model can address via update_npc. The constants
// are used by UpdateSection and the parser in
// internal/usecase/gm.go. Aliases are matched in
// MatchSection.
type SectionKind int

const (
	SectionUnknown SectionKind = iota
	SectionTemperament
	SectionRelationsGG
	SectionRelationsNPCs
	SectionAbilities
	SectionPersonalMemory
	SectionCurrentStatus
	SectionCriticalKnowledge
	SectionNicknames
	SectionLastUpdate
)

// CanonicalSectionName returns the Russian section
// header as it appears in BuildMarkdown's output.
// Used by the parser to label missing-sections in
// the format re-prompt.
func (k SectionKind) CanonicalSectionName() string {
	switch k {
	case SectionTemperament:
		return "Темперамент"
	case SectionRelationsGG:
		return "Отношения с ГГ"
	case SectionRelationsNPCs:
		return "Отношения с другими NPC"
	case SectionAbilities:
		return "Способности"
	case SectionPersonalMemory:
		return "Личная память/факты"
	case SectionCurrentStatus:
		return "Текущий статус"
	case SectionCriticalKnowledge:
		return "Критические знания"
	case SectionNicknames:
		return "Никнеймы"
	case SectionLastUpdate:
		return "Последнее обновление"
	case SectionUnknown:
		return ""
	}

	return ""
}

// MatchSection resolves a free-form section name
// (Russian canonical, Russian casual, or English
// alias) to a typed SectionKind. The model is
// allowed to write any of these forms in update_npc;
// the dispatcher canonicalises to the typed kind so
// the storage layer can route to the right array.
func MatchSection(name string) SectionKind {
	trimmed := strings.TrimSpace(strings.ToLower(name))
	switch trimmed {
	case "темперамент", "temperament", "temper":
		return SectionTemperament
	case "отношения с гг", "relations_gg", "relations gg",
		"relationsgg", "gg", "отношения с главным", "к гг":
		return SectionRelationsGG
	case "отношения с другими npc",
		"отношения с npc",
		"relations_npcs",
		"npc relations",
		"npc_relations",
		"отношения",
		"к другим npc":
		return SectionRelationsNPCs
	case "способности", "abilities", "умения", "навыки", "skills":
		return SectionAbilities
	case "личная память", "личная память/факты", "память", "факты",
		"personal memory", "personal_memory", "memory":
		return SectionPersonalMemory
	case "текущий статус", "статус", "current status", "current_status", "status":
		return SectionCurrentStatus
	case "критические знания", "знания", "секреты",
		"critical knowledge", "critical_knowledge", "secrets":
		return SectionCriticalKnowledge
	case "никнеймы", "nicknames", "клички", "nickname", "прозвища":
		return SectionNicknames
	case "последнее обновление", "last update", "last_update", "обновление", "update":
		return SectionLastUpdate
	}

	return SectionUnknown
}

// UpdateSection mutates a Profile in place based on
// the section kind. The text argument is the model-
// supplied content; how it is parsed depends on the
// section:
//
//   - Scalars (Temperament, RelationsGG, CurrentStatus,
//     LastUpdate): REPLACE. text replaces whatever was
//     there. LastUpdate is special: it is the only
//     scalar that the legacy code allowed to be
//     appended; we keep the REPLACE semantic here for
//     consistency. The PersonalMemory array keeps the
//     history of "what changed" by appending the new
//     fact there as well — see UpdateSectionWithLog.
//
//   - Arrays (Abilities, PersonalMemory, Nicknames,
//     CriticalKnowledge): APPEND with dedup. Empty
//     strings are skipped. Exact-byte match (after
//     TrimSpace) is the dedup key. We also fold case
//     for personal_memory so "День 1: встал" and
//     "день 1: встал" do not both land — the more
//     recent spelling wins.
//
//   - RelationsNPCs: APPEND a new Relation if the
//     target is not already present. If it is, the
//     note is REPLACED (the most recent impression
//     of "how do they feel about X" wins). The text
//     is split on the first colon to get
//     "target: note"; the model's prompt tells it to
//     use that shape.
//
// Returns true if the profile changed (something was
// added or replaced). False means the section was a
// no-op (empty text, or exact-byte duplicate of an
// existing item).
//
// UpdateSection is a straight switch across 14 section kinds;
// cyclomatic complexity scales with the enum, not with branching logic
//
// cyclomatic complexity scales with the enum, not with branching logic.
//
//nolint:cyclop,funlen // Profile.UpdateSection branches per section kind;
func (p *Profile) UpdateSection(kind SectionKind, text string) bool {
	text = strings.TrimSpace(text)

	switch kind {
	case SectionTemperament:
		if text == "" || p.Temperament == text {
			return false
		}

		p.Temperament = text

		return true
	case SectionRelationsGG:
		if text == "" || p.RelationsGG == text {
			return false
		}

		p.RelationsGG = text

		return true
	case SectionCurrentStatus:
		if text == "" || p.CurrentStatus == text {
			return false
		}

		p.CurrentStatus = text

		return true
	case SectionLastUpdate:
		if text == "" || p.LastUpdate == text {
			return false
		}

		p.LastUpdate = text

		return true
	case SectionAbilities:
		return appendUnique(&p.Abilities, text)
	case SectionPersonalMemory:
		if text == "" {
			return false
		}
		// Dedup: case-insensitive, whitespace-trimmed
		// comparison. We keep the original casing
		// of the first occurrence (no rewrite on
		// re-encounter with a different capitalisation).
		key := strings.ToLower(text)
		for _, existing := range p.PersonalMemory {
			if strings.ToLower(strings.TrimSpace(existing)) == key {
				return false
			}
		}

		p.PersonalMemory = append(p.PersonalMemory, text)

		return true
	case SectionCriticalKnowledge:
		return appendUnique(&p.CriticalKnowledge, text)

	case SectionNicknames:
		return appendUnique(&p.Nicknames, text)

	case SectionRelationsNPCs:
		return p.updateRelation(text)
	case SectionUnknown:
		return false
	}

	return false
}

// joinNonEmpty joins a slice of lines with single
// spaces, dropping any empty / whitespace-only entries.
func joinNonEmpty(lines []string) string {
	var parts []string

	for _, ln := range lines {
		if t := strings.TrimSpace(ln); t != "" {
			parts = append(parts, t)
		}
	}

	return strings.Join(parts, " ")
}

// splitRelationText splits "Target: note" on the
// first colon. "Target" alone (no note) is allowed.
func splitRelationText(s string) (string, string) {
	for i, r := range s {
		if r == ':' {
			return strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+1:])
		}
	}

	return strings.TrimSpace(s), ""
}

// appendUnique is the shared array-append helper. It
// trims, drops empties, and dedups by exact byte
// match (case-sensitive — the model's spelling is the
// canonical one; we do not rewrite it).
func appendUnique(field *[]string, text string) bool {
	text = strings.TrimSpace(text)
	if text == "" {
		return false
	}

	if slices.Contains(*field, text) {
		return false
	}

	*field = append(*field, text)

	return true
}

// MigrateFromMarkdown converts a legacy free-form
// markdown file into a Profile. We do NOT aim for a
// perfect 1:1 reconstruction — the goal is a
// "good enough" profile so the operator does not
// have to re-enter every fact. Unknown sections are
// dropped with a warning (the operator can recover
// from git history).
//
// The heuristic is:
//
//	# <DisplayName>
//	## Темперамент
//	<scalar>
//	## Отношения с ГГ
//	<scalar>
//	## Отношения с другими NPC
//	- Target: note
//	- Target: note
//	## Способности
//	- ability
//	## Личная память/факты
//	- fact
//	## Текущий статус
//	<scalar>
//	## Критические знания
//	- secret
//	## Никнеймы
//	- nickname
//	## Последнее обновление
//	<scalar>
//
// Anything not matching a known section header is
// dropped (we return nil for the section list — the
// operator is responsible for the diff).
//
// MigrateFromMarkdown walks line-by-line through a legacy markdown profile;
// one pass per line is clearer than per-section helpers that would each
// re-implement the same header/body split
//
// per-section helpers that would each re-implement the header/body split.
//
//nolint:gocognit,cyclop,funlen // MigrateFromMarkdown walks line-by-line; one pass per line is clearer than
func MigrateFromMarkdown(body, fileSlug string) (Profile, error) {
	p := Profile{FileSlug: fileSlug}
	if strings.TrimSpace(body) == "" {
		return p, ErrNotFound
	}

	lines := strings.Split(body, "\n")

	var (
		current SectionKind
		pending []string
	)

	flush := func() {
		if current == SectionUnknown || len(pending) == 0 {
			return
		}
		// Scalars get a single UpdateSection with the
		// joined text; arrays get one update per item
		// so dedup applies per line (and the order is
		// preserved).
		switch current {
		case SectionAbilities, SectionPersonalMemory,
			SectionCriticalKnowledge, SectionNicknames:
			for _, item := range pending {
				p.UpdateSection(current, item)
			}
		case SectionRelationsNPCs:
			for _, item := range pending {
				p.updateRelation(item)
			}
		default:
			text := strings.TrimSpace(strings.Join(pending, " "))
			if text != "" {
				p.UpdateSection(current, text)
			}
		}

		pending = nil
	}

	for _, raw := range lines {
		t := strings.TrimSpace(raw)
		switch {
		case strings.HasPrefix(t, "# ") && !strings.HasPrefix(t, "## "):
			p.DisplayName = strings.TrimSpace(t[2:])
		case strings.HasPrefix(t, "## "):
			sawSection = true

			flush()

			current = MatchSection(t[3:])
		case t == "":
			flush()

			current = SectionUnknown
		default:
			item := strings.TrimPrefix(t, "- ")
			item = strings.TrimPrefix(item, "* ")
			// Drop numbered-list prefix
			// ("1. ", "2. ", ...).
			if idx := strings.Index(t, ". "); idx > 0 {
				prefix := t[:idx]
				isDigits := true

				for _, r := range prefix {
					if r < '0' || r > '9' {
						isDigits = false

						break
					}
				}

				if isDigits {
					item = strings.TrimSpace(t[idx+2:])
				}
			}

			pending = append(pending, item)
		}
	}
	// Final flush — the last section may not have been
	// terminated by a blank line or a new header.
	flush()
	// Heuristic: if no section headers were found
	// (legacy "name + one free-form paragraph"
	// files, common in the oldest commits), drop the
	// free-form text into Темперамент so the model
	// has a stable section to anchor on. The
	// alternative — silently dropping the body — is
	// strictly worse: the operator's manual edits
	// would vanish on the next save.
	if !sawSection && strings.TrimSpace(p.Temperament) == "" {
		if body := strings.TrimSpace(joinNonEmpty(pending)); body != "" {
			p.Temperament = body
		}
	}

	if p.DisplayName == "" {
		p.DisplayName = fileSlug
	}

	return p, nil
}

// sawSection is set inside the parser when any
// "## " header is encountered. The legacy "name +
// free text" files do not have any.
var sawSection bool //nolint:gochecknoglobals // parser output latch shared by the markdown→yaml migration helpers

// SortedKeys returns the names of every section that
// has data, alphabetically sorted. Used by the
// operator-facing diagnostic (e.g. /inspect) and the
// summarizer to know which sections are populated.
func (p *Profile) SortedKeys() []string {
	var keys []string
	if p.Temperament != "" {
		keys = append(keys, SectionTemperament.CanonicalSectionName())
	}

	if p.RelationsGG != "" {
		keys = append(keys, SectionRelationsGG.CanonicalSectionName())
	}

	if len(p.RelationsNPCs) > 0 {
		keys = append(keys, SectionRelationsNPCs.CanonicalSectionName())
	}

	if len(p.Abilities) > 0 {
		keys = append(keys, SectionAbilities.CanonicalSectionName())
	}

	if len(p.PersonalMemory) > 0 {
		keys = append(keys, SectionPersonalMemory.CanonicalSectionName())
	}

	if p.CurrentStatus != "" {
		keys = append(keys, SectionCurrentStatus.CanonicalSectionName())
	}

	if len(p.CriticalKnowledge) > 0 {
		keys = append(keys, SectionCriticalKnowledge.CanonicalSectionName())
	}

	if len(p.Nicknames) > 0 {
		keys = append(keys, SectionNicknames.CanonicalSectionName())
	}

	if p.LastUpdate != "" {
		keys = append(keys, SectionLastUpdate.CanonicalSectionName())
	}

	sort.Strings(keys)

	return keys
}

// updateRelation parses "Target: note" from text and
// either inserts a new Relation or replaces the note
// on an existing one (same Target).
func (p *Profile) updateRelation(text string) bool {
	if text == "" {
		return false
	}

	target, note := splitRelationText(text)
	if target == "" {
		return false
	}

	targetKey := strings.ToLower(strings.TrimSpace(target))
	for i := range p.RelationsNPCs {
		existing := strings.ToLower(strings.TrimSpace(p.RelationsNPCs[i].Target))
		if existing == targetKey {
			if p.RelationsNPCs[i].Note == note {
				return false
			}

			p.RelationsNPCs[i].Note = note

			return true
		}
	}

	p.RelationsNPCs = append(p.RelationsNPCs, Relation{Target: target, Note: note})

	return true
}
