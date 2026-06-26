package yaml

import (
	"fmt"
	"strings"

	"github.com/bestxp/narrative-ai-agent/internal/npcprofile"
	"github.com/bestxp/narrative-ai-agent/internal/storage"
)

// npcProfileKey returns the storage key for one
// NPC's profile YAML.
func npcProfileKey(world, slug string) string {
	return "worlds/" + world + "/characters/" + slug + ".yaml"
}

// NPCProfileYaml is the YAML-backed implementation of
// NPCProfileRepository. Each NPC has its own YAML file
// under worlds/<w>/characters/<slug>.yaml.
//
// The NPC registry (worlds/<w>/characters.yaml) used
// to live here as NPCRegistryYaml — the load path also
// had a characters.md fallback. Both were removed:
// characters.yaml is the only canonical roster and
// is read/written through the worldregistry package.
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
		return nil, fmt.Errorf("list_slugs: ListChildren failed: %w", err)
	}

	out := make([]string, 0, len(entries))
	for _, name := range entries {
		if slug, ok := strings.CutSuffix(name, ".yaml"); ok {
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
		return npcprofile.Profile{}, fmt.Errorf("npc_load: Read failed: %w", err)
	}

	if strings.TrimSpace(string(body)) == "" {
		return npcprofile.Profile{}, npcprofile.ErrNotFound
	}

	p, err := npcprofile.Load(string(body))
	if err != nil {
		return npcprofile.Profile{}, fmt.Errorf("load: Load failed: %w", err)
	}

	return p, nil
}

// Save persists the profile as YAML.
func (r *NPCProfileYaml) Save(world, slug string, p npcprofile.Profile) error {
	body, err := p.Save()
	if err != nil {
		return fmt.Errorf("save: Save failed: %w", err)
	}

	if err := r.store.Write(npcProfileKey(world, slug), []byte(body)); err != nil {
		return fmt.Errorf("save: write: %w", err)
	}

	return nil
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
