package api

import "github.com/bestxp/narrative-ai-agent/internal/npcprofile"

// NPCProfileRepository owns a world's NPC files
// (one YAML per NPC under worlds/<w>/characters/<slug>.yaml).
type NPCProfileRepository interface {
	// ListSlugs returns the slugs (filenames without
	// .yaml) of every NPC under world's characters/
	// directory. Used by MaintainNPCs to walk the
	// roster; implementations may return (nil, nil)
	// for a missing directory.
	ListSlugs(world string) ([]string, error)
	// Load returns the parsed profile, the file slug
	// (== name without extension), or ErrNotFound.
	Load(world, slug string) (npcprofile.Profile, error)
	Save(world, slug string, p npcprofile.Profile) error
	// UpdateSection mutates the named section in place.
	// Returns true if the file changed. The model calls
	// update_npc with section + appendText; the repo
	// handles dedup, replace, and relation upsert.
	UpdateSection(world, slug, section, appendText string) (bool, error)
}

// NPCRegistryRepository owns the world's NPC registry
// (worlds/<w>/characters.yaml) — a compact list of every
// NPC known to the world (slug + display_name + nicknames)
// used by gm.loadActiveNPCs.
type NPCRegistryRepository interface {
	Load(world string) (string, error)
	Save(world, body string) error
	// AppendEntry adds a new NPC to the registry. Used
	// by create_npc after the per-NPC YAML is written.
	AppendEntry(world, slug, displayName string, nicknames []string) error
}
