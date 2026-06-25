// Package worldregistry is the per-world directory
// of NPCs. The bot's "Хината — статус: идёт домой"
// directive does not name a file — it names a
// character. Translating that to a file path is
// this package's job.
//
// Storage layout per world:
//
//	worlds/<world>/characters.yaml   ← canonical registry
//	worlds/<world>/characters/<slug>.yaml   ← per-NPC profile
//
// characters.yaml is the source of truth. Each
// entry maps a display_name (and any nicknames)
// to a file slug. The model refers to NPCs by
// display_name in every КОНТЕКСТ directive; the
// registry is what makes that resolution
// deterministic regardless of how the operator
// chose to spell the file.
//
// Earlier revisions kept a `characters.md` fallback
// for worlds where the operator had hand-edited a
// markdown table. That fallback was removed: it
// produced duplicate-NPC cases where one registry
// listed a character that the other did not. The
// registry is now characters.yaml and nothing else;
// create_npc is the only path that writes to it.
//
// If a name is not in the registry, Lookup returns
// (slug="", ok=false) — the caller (UpdateNPC /
// Load) is then expected to surface a "create_npc
// first" signal so the GM can trigger Create.
package worldregistry

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// ErrNotFound is returned by Lookup when the
// display_name is not in the registry. The caller
// should treat this as a soft signal — the NPC
// might just not have been introduced yet.
var ErrNotFound = errors.New("worldregistry: npc not in registry")

// Registry is the in-memory mirror of
// worlds/<w>/characters.yaml.
type Registry struct {
	// entries are kept sorted by slug for stable
	// diffs. Slug is the file basename without
	// the .yaml extension; it must be unique
	// within a world.
	entries []Entry
}

// Entry is one row of characters.yaml.
type Entry struct {
	// Slug is the file basename without ".yaml".
	// The NPC profile lives at
	// worlds/<w>/characters/<Slug>.yaml.
	Slug string `yaml:"slug"`
	// DisplayName is the human-readable name the
	// model uses in КОНТЕКСТ directives
	// ("Хината", "Ирука-сенсей"). Matched
	// case-insensitively, trimmed.
	DisplayName string `yaml:"display_name"`
	// Nicknames are short aliases the model
	// commonly uses ("Хината-чан", "сенсей",
	// "Куро"). Each nickname is matched
	// case-insensitively, trimmed. Substring
	// matches within a single name field are
	// accepted (see Lookup).
	Nicknames []string `yaml:"nicknames,omitempty"`
}

// registryFile is the on-disk shape. Kept private
// so callers go through Registry methods.
type registryFile struct {
	NPCs []Entry `yaml:"npcs"`
}

// Load reads the registry for world from fs.
//
// Only characters.yaml is read. There is no
// characters.md fallback: the canonical roster is
// one file, owned by this package, and an empty
// world returns an empty Registry (the first NPC
// to be created will seed it).
func Load(fs interface {
	ReadRaw(rel string) (string, error)
	WriteRawAtomic(rel, body string) error
	Exists(rel string) bool
}, world string) (*Registry, error) {
	if world == "" {
		return nil, errors.New("worldregistry: world is empty")
	}
	rel := "worlds/" + world + "/characters.yaml"
	body, err := fs.ReadRaw(rel)
	if err != nil {
		return &Registry{}, nil
	}
	if strings.TrimSpace(body) == "" {
		return &Registry{}, nil
	}
	var f registryFile
	if uerr := yaml.Unmarshal([]byte(body), &f); uerr != nil {
		return nil, fmt.Errorf("worldregistry: parse %s: %w", rel, uerr)
	}
	r := &Registry{entries: append([]Entry(nil), f.NPCs...)}
	r.sort()
	return r, nil
}

// Save serialises the registry to YAML. The output
// is sorted by slug for stable diffs in git.
func (r *Registry) Save() (string, error) {
	r.sort()
	f := registryFile{NPCs: append([]Entry(nil), r.entries...)}
	out, err := yaml.Marshal(f)
	if err != nil {
		return "", fmt.Errorf("worldregistry: marshal: %w", err)
	}
	return string(out), nil
}

// Lookup resolves a model-supplied NPC name to
// the registry entry. Match priority:
//
//  1. Exact slug match (case-insensitive). The
//     model occasionally writes the file name
//     instead of the display_name ("naruto_uzumaki").
//  2. Exact display_name match (case-insensitive,
//     trimmed).
//  3. Exact nickname match.
//  4. Substring match: the model's token is a
//     case-insensitive substring of one of the
//     candidate's fields (or vice versa). Only
//     applied when the result is unambiguous
//     (single hit) — substring matching against
//     multiple files is too loose and would
//     mis-route. The "Хината" → "Хината Хьюга"
//     case is the prime example: the model
//     rarely writes the full surname.
//
// Returns (entry, true) on a hit, (zero, false)
// when nothing matched. Callers should treat
// "not found" as a prompt to call Create — the
// model will get a fresh "create_npc first" error
// and can retry with the new tool call.
func (r *Registry) Lookup(name string) (Entry, bool) {
	want := strings.ToLower(strings.TrimSpace(name))
	if want == "" {
		return Entry{}, false
	}
	// Stage 1: exact (slug / display_name / nickname).
	for _, e := range r.entries {
		if strings.EqualFold(e.Slug, want) ||
			strings.EqualFold(strings.TrimSpace(e.DisplayName), want) {
			return e, true
		}
		for _, n := range e.Nicknames {
			if strings.EqualFold(strings.TrimSpace(n), want) {
				return e, true
			}
		}
	}
	// Stage 2: substring (case-insensitive,
	// unambiguous). Two candidates: either the
	// query is a substring of the candidate's
	// field, or the candidate's field is a
	// substring of the query. The latter is the
	// "Хината Хьюга" → "Хината" direction.
	var hit Entry
	ambiguous := false
	for _, e := range r.entries {
		if matchAnyField(e, want) {
			if hit.Slug == "" {
				hit = e
			} else {
				ambiguous = true
			}
		}
	}
	if hit.Slug != "" && !ambiguous {
		return hit, true
	}
	return Entry{}, false
}

// Add appends a new entry. Returns an error if
// the slug is already taken.
func (r *Registry) Add(e Entry) error {
	e.Slug = strings.TrimSpace(e.Slug)
	e.DisplayName = strings.TrimSpace(e.DisplayName)
	if e.Slug == "" {
		return errors.New("worldregistry: empty slug")
	}
	for _, ex := range r.entries {
		if strings.EqualFold(ex.Slug, e.Slug) {
			return fmt.Errorf("worldregistry: slug %q already in registry", e.Slug)
		}
	}
	r.entries = append(r.entries, e)
	r.sort()
	return nil
}

// All returns a copy of the entries in slug order.
// Used by the operator-facing diagnostic and by
// the system prompt (the model sees a list of
// known NPCs so it does not invent display_names
// for characters that do not exist yet).
func (r *Registry) All() []Entry {
	out := make([]Entry, len(r.entries))
	copy(out, r.entries)
	return out
}

// sort orders entries by slug. Stable across
// Save/Load so the on-disk diff is minimal.
func (r *Registry) sort() {
	sort.SliceStable(r.entries, func(i, j int) bool {
		return r.entries[i].Slug < r.entries[j].Slug
	})
}

// matchAnyField reports whether want is a
// case-insensitive substring of the entry's
// display_name or any nickname, OR vice versa.
func matchAnyField(e Entry, want string) bool {
	if strings.Contains(strings.ToLower(e.DisplayName), want) {
		return true
	}
	for _, n := range e.Nicknames {
		if strings.Contains(strings.ToLower(n), want) {
			return true
		}
	}
	if e.DisplayName != "" && strings.Contains(want, strings.ToLower(e.DisplayName)) {
		return true
	}
	for _, n := range e.Nicknames {
		if n != "" && strings.Contains(want, strings.ToLower(n)) {
			return true
		}
	}
	return false
}
