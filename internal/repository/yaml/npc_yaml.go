package yaml

import (
	"regexp"
	"strings"

	"github.com/bestxp/narrative-ai-agent/internal/npcprofile"
	"github.com/bestxp/narrative-ai-agent/internal/storage"
)

// npcProfileKey returns the storage key for one
// NPC's profile YAML.
func npcProfileKey(world, slug string) string {
	return "worlds/" + world + "/characters/" + slug + ".yaml"
}

// npcRegistryKey returns the storage key for the
// world's NPC registry (markdown table).
func npcRegistryKey(world string) string {
	return "worlds/" + world + "/characters.md"
}

// NPCProfileYaml is the YAML-backed implementation of
// NPCProfileRepository. Each NPC has its own YAML file
// under worlds/<w>/characters/<slug>.yaml.
type NPCProfileYaml struct {
	store storage.Storage
}

// NewNPCProfileYaml constructs the NPC profile
// repository.
func NewNPCProfileYaml(store storage.Storage) *NPCProfileYaml {
	return &NPCProfileYaml{store: store}
}

// ListSlugs returns the per-NPC filenames (without
// .yaml) under worlds/<w>/characters/. Returns
// (nil, nil) when the directory does not exist
// (a fresh world has no characters yet).
func (r *NPCProfileYaml) ListSlugs(world string) ([]string, error) {
	entries, err := r.store.ListChildren("worlds/" + world + "/characters")
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, name := range entries {
		if strings.HasSuffix(name, ".yaml") {
			slug := strings.TrimSuffix(name, ".yaml")
			out = append(out, slug)
		}
	}
	return out, nil
}

// Load returns the parsed profile. Empty body
// returns ErrNotFound — the dispatcher surfaces this
// to the model as "NPC does not exist yet, call
// create_npc first".
func (r *NPCProfileYaml) Load(world, slug string) (npcprofile.Profile, error) {
	body, err := r.store.Read(npcProfileKey(world, slug))
	if err != nil {
		return npcprofile.Profile{}, err
	}
	if strings.TrimSpace(string(body)) == "" {
		return npcprofile.Profile{}, npcprofile.ErrNotFound
	}
	p, err := npcprofile.Load(string(body))
	if err != nil {
		return npcprofile.Profile{}, err
	}
	return p, nil
}

// Save persists the profile as YAML.
func (r *NPCProfileYaml) Save(world, slug string, p npcprofile.Profile) error {
	body, err := p.Save()
	if err != nil {
		return err
	}
	return r.store.Write(npcProfileKey(world, slug), []byte(body))
}

// UpdateSection mutates the named section in place
// via the entity's section-update path. Returns true
// if the file changed.
//
// The repository delegates the section-name →
// SectionKind mapping to npcprofile.MatchSection.
// Repository code does NOT know which sections exist;
// that knowledge lives in the entity package.
func (r *NPCProfileYaml) UpdateSection(world, slug, section, appendText string) (bool, error) {
	p, err := r.Load(world, slug)
	if err != nil {
		return false, err
	}
	kind := npcprofile.MatchSection(section)
	if kind == npcprofile.SectionUnknown {
		return false, nil
	}
	if !p.UpdateSection(kind, appendText) {
		return false, nil
	}
	if err := r.Save(world, slug, p); err != nil {
		return false, err
	}
	return true, nil
}

// --- NPC REGISTRY ---

// NPCRegistryYaml is the YAML/markdown-backed
// implementation of NPCRegistryRepository. The
// registry is a compact markdown table with columns
// "Имя | Файл | Прозвища".
type NPCRegistryYaml struct {
	store storage.Storage
}

// NewNPCRegistryYaml constructs the NPC registry
// repository.
func NewNPCRegistryYaml(store storage.Storage) *NPCRegistryYaml {
	return &NPCRegistryYaml{store: store}
}

// Load returns the raw registry body or "" if missing.
func (r *NPCRegistryYaml) Load(world string) (string, error) {
	body, err := r.store.Read(npcRegistryKey(world))
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// Save persists the registry body.
func (r *NPCRegistryYaml) Save(world, body string) error {
	return r.store.Write(npcRegistryKey(world), []byte(body))
}

// AppendEntry adds a new row to the registry
// markdown table. The format is:
//
//	| <display_name> | <slug>.yaml | <comma-separated nicknames> |
//
// Used by create_npc after the per-NPC YAML is
// written.
func (r *NPCRegistryYaml) AppendEntry(world, slug, displayName string, nicknames []string) error {
	current, err := r.Load(world)
	if err != nil {
		return err
	}
	var nickCol string
	if len(nicknames) > 0 {
		nickCol = strings.Join(nicknames, ", ")
	}
	row := "| " + displayName + " | " + slug + ".yaml | " + nickCol + " |"
	if current == "" {
		header := "# NPC: " + world + "\n| Имя | Файл | Прозвища |\n|-----|------|----------|\n"
		return r.Save(world, header+row+"\n")
	}
	if !strings.HasSuffix(current, "\n") {
		current += "\n"
	}
	return r.Save(world, current+row+"\n")
}

// registryRowRe matches the per-NPC row written by
// AppendEntry. Used by the operator's /inspect
// command to parse the table.
var registryRowRe = regexp.MustCompile(`^\|\s*([^|]+?)\s*\|\s*([^|]+?)\s*\|\s*([^|]*?)\s*\|$`)

// ParseRegistryRow parses one row into
// (displayName, slug, nicknames).
func ParseRegistryRow(line string) (displayName, slug string, nicknames []string, ok bool) {
	m := registryRowRe.FindStringSubmatch(line)
	if m == nil {
		return "", "", nil, false
	}
	displayName = strings.TrimSpace(m[1])
	slugFile := strings.TrimSpace(m[2])
	if strings.HasSuffix(slugFile, ".yaml") {
		slug = strings.TrimSuffix(slugFile, ".yaml")
	} else {
		slug = slugFile
	}
	nickCol := strings.TrimSpace(m[3])
	if nickCol != "" {
		for _, n := range strings.Split(nickCol, ",") {
			if v := strings.TrimSpace(n); v != "" {
				nicknames = append(nicknames, v)
			}
		}
	}
	return displayName, slug, nicknames, true
}

// its corresponding repository.XxxRepository. The
// matching assertion lives in repository/contracts.go
// (which can import yaml/, but yaml/ cannot import
// the parent package — that would cycle).
