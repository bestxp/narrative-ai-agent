package files

import (
	"errors"
	"fmt"
	"strings"

	"github.com/bestxp/narrative-ai-agent/internal/adapter/storage"
	"github.com/bestxp/narrative-ai-agent/internal/domain"
	"github.com/bestxp/narrative-ai-agent/internal/npcprofile"
	"github.com/bestxp/narrative-ai-agent/internal/repository/api"
	"github.com/bestxp/narrative-ai-agent/internal/slowlog"
	"github.com/bestxp/narrative-ai-agent/internal/usecase/tools"
	"github.com/bestxp/narrative-ai-agent/internal/worldregistry"
	"github.com/rs/zerolog"
)

// NPC is the repository-backed implementation of
// tools.NPCTool: create on first appearance, plus
// the knowledge-isolation helper that filters a
// candidate reply down to what the active NPC may say.
//
// Per-NPC files live under worlds/<w>/characters/<slug>.yaml.
// The roster (worlds/<w>/characters.yaml) is read and
// written through the worldregistry package — see
// that package for the lookup rules (exact / nickname /
// unambiguous substring).
type NPC struct {
	repos *api.Repositories
	fs    *storage.FileStore
	log   zerolog.Logger
	slow  *slowlog.Logger
}

func newNPC(log zerolog.Logger, slow *slowlog.Logger, repos *api.Repositories, fs *storage.FileStore) *NPC {
	return &NPC{
		repos: repos,
		fs:    fs,
		log:   log.With().Str("component", "npc").Logger(),
		slow:  slow,
	}
}

// ErrNPCExists is returned when Create is called for
// an NPC that already has a file.
var ErrNPCExists = errors.New("npc file already exists")

// ErrNPCNotFound is returned when Load or UpdateNPC
// is called for an NPC that has no file yet.
var ErrNPCNotFound = errors.New("npc: file not found; call create_npc first")

// SanitizedName is a slug-friendly version of display
// name — latin-only, lowercase, hyphens for spaces.
// Used as the on-disk filename (without .yaml).
func SanitizedName(display string) string {
	return sanitizeName(display)
}

func sanitizeName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, " ", "-")
	// Transliterate Cyrillic → Latin.
	s = domain.Transliterate(s)
	// Drop anything that's not [a-z0-9-].
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-':
			b.WriteRune(r)
		}
	}
	out := b.String()
	out = strings.Trim(out, "-")
	if out == "" {
		return "npc"
	}
	return out
}

// loadRegistry reads characters.yaml for world through the
// worldregistry package. On any parse or read error it logs
// and returns an empty registry — Load / UpdateNPC then
// surface "npc not found" to the model, which is the
// correct recovery path (model creates the NPC and retries).
func (n *NPC) loadRegistry(world string) *worldregistry.Registry {
	if n.fs == nil || world == "" {
		return &worldregistry.Registry{}
	}
	r, err := worldregistry.Load(n.fs, world)
	if err != nil {
		n.log.Warn().Err(err).Str("world", world).Msg("npc: registry load failed; treating as empty")
		return &worldregistry.Registry{}
	}
	return r
}

// saveRegistry persists the registry through the file
// store. Save errors are logged; the caller may still
// return success on the per-NPC file write so the rest
// of create_npc proceeds (a stale roster is recoverable
// on the next load).
func (n *NPC) saveRegistry(world string, r *worldregistry.Registry) {
	if n.fs == nil || world == "" {
		return
	}
	body, err := r.Save()
	if err != nil {
		n.log.Warn().Err(err).Str("world", world).Msg("npc: registry marshal failed")
		return
	}
	if err := n.fs.WriteRawAtomic("worlds/"+world+"/characters.yaml", body); err != nil {
		n.log.Warn().Err(err).Str("world", world).Msg("npc: registry write failed")
	}
}

// Create writes a new NPC profile to the world's
// characters directory via repos.NPCProfile and adds
// the entry to the YAML registry (characters.yaml).
// Returns ErrNPCExists if the slug is already taken.
func (n *NPC) Create(world string, p tools.NPCProfile) error { //nolint:funlen // complex function; splitting would harm readability.
	slug := sanitizeName(p.DisplayName)
	if slug == "" {
		return errors.New("npc create: empty display name")
	}

	// Check for existing file.
	_, err := n.repos.NPCProfile.Load(world, slug)
	if err == nil {
		return ErrNPCExists
	}
	if !errors.Is(err, npcprofile.ErrNotFound) {
		return fmt.Errorf("create_npc: NPCProfile.Load failed: %w", err)
	}

	// Build the canonical profile.
	profile := npcprofile.Profile{
		DisplayName:       strings.TrimSpace(p.DisplayName),
		FileSlug:          slug,
		Temperament:       strings.TrimSpace(p.Temperament),
		RelationsGG:       "", // legacy free-text — populated via UpdateNPC
		CurrentStatus:     strings.TrimSpace(p.CurrentStatus),
		CriticalKnowledge: nil,
		Nicknames:         p.Nicknames,
	}
	if strings.TrimSpace(p.Relations) != "" {
		// Legacy free-text relations — split by lines into
		// the structured RelationsNPCs array.
		for line := range strings.SplitSeq(p.Relations, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			target, note := splitRelationText(line)
			if target != "" {
				profile.RelationsNPCs = append(profile.RelationsNPCs, npcprofile.Relation{
					Target: target,
					Note:   note,
				})
			}
		}
	}
	if strings.TrimSpace(p.PersonalMemory) != "" {
		for line := range strings.SplitSeq(p.PersonalMemory, "\n") {
			if t := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "- ")); t != "" {
				profile.PersonalMemory = append(profile.PersonalMemory, t)
			}
		}
	}
	if len(p.Abilities) > 0 {
		for _, a := range p.Abilities {
			if t := strings.TrimSpace(strings.TrimPrefix(a, "- ")); t != "" {
				profile.Abilities = append(profile.Abilities, t)
			}
		}
	}
	if strings.TrimSpace(p.CriticalKnowledge) != "" {
		for line := range strings.SplitSeq(p.CriticalKnowledge, "\n") {
			if t := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "- ")); t != "" {
				profile.CriticalKnowledge = append(profile.CriticalKnowledge, t)
			}
		}
	}
	profile.LastUpdate = strings.TrimSpace(p.LastUpdate)

	if err := n.repos.NPCProfile.Save(world, slug, profile); err != nil {
		return fmt.Errorf("create_npc: NPCProfile.Save failed: %w", err)
	}

	// Append to the YAML registry. We dedup on slug via
	// Registry.Add (returns error on duplicate) so a
	// stale per-NPC file + missing registry row (the
	// case that creates "npc: file not found" today)
	// is self-healing: create_npc just refreshes the
	// registry row.
	r := n.loadRegistry(world)
	if err := r.Add(worldregistry.Entry{
		Slug:        slug,
		DisplayName: profile.DisplayName,
		Nicknames:   profile.Nicknames,
	}); err != nil {
		// Slug already in registry (stale row from a
		// prior run): reload after Add fails so the
		// saved file still has the latest entry
		// shape, then proceed.
		n.log.Debug().Err(err).Str("slug", slug).Msg("create_npc: registry.Add duplicate; refreshing")
	}
	n.saveRegistry(world, r)

	n.log.Info().
		Str("world", world).
		Str("npc", profile.DisplayName).
		Str("slug", slug).
		Msg("create_npc")
	return nil
}

// Load returns the full markdown render of an NPC
// profile via repos.NPCProfile. For multi-NPC scenes
// where the cache budget is tight, prefer LoadLOD.
func (n *NPC) Load(world, npc string) (string, error) {
	return n.LoadLOD(world, npc, tools.LODFull)
}

// LoadLOD returns the NPC profile rendered at the
// requested level of detail. LODFull = BuildMarkdown
// (every section); LODCompact = BuildCompact (drop
// big arrays); LODOneLine = BuildOneLine (name +
// 1-sentence temperament + status).
//
// The npc argument may be a slug, a display name, or a
// nickname; the registry's Lookup handles the resolution
// (exact match preferred, then unambiguous substring).
func (n *NPC) LoadLOD(world, npc string, lod tools.NPCLOD) (string, error) {
	slug, err := n.resolveSlug(world, npc)
	if err != nil {
		return "", err
	}
	profile, err := n.repos.NPCProfile.Load(world, slug)
	if err != nil {
		if errors.Is(err, npcprofile.ErrNotFound) {
			return "", ErrNPCNotFound
		}
		return "", err
	}

	switch lod {
	case tools.LODFull:
		body, err := profile.BuildMarkdown()
		if err != nil {
			return "", fmt.Errorf("load_lod: BuildMarkdown failed: %w", err)
		}
		return body, nil
	case tools.LODCompact:
		return profile.BuildCompact(), nil
	case tools.LODOneLine:
		return profile.BuildOneLine(), nil
	}
	body, err := profile.BuildMarkdown()
	if err != nil {
		return "", fmt.Errorf("load_lod: BuildMarkdown failed: %w", err)
	}
	return body, nil
}

// UpdateNPC appends fresh facts to an existing NPC
// profile via repos.NPCProfile.UpdateSection.
func (n *NPC) UpdateNPC(world, npc, section, appendText string) error {
	slug, err := n.resolveSlug(world, npc)
	if err != nil {
		return fmt.Errorf("update_npc: resolveSlug failed: %w", err)
	}
	ok, err := n.repos.NPCProfile.UpdateSection(world, slug, section, appendText)
	if err != nil {
		return fmt.Errorf("update_npc: UpdateSection failed: %w", err)
	}
	if !ok {
		n.log.Debug().
			Str("world", world).
			Str("npc", npc).
			Str("section", section).
			Msg("update_npc: no change (dedup or unknown section)")
		return nil
	}
	n.log.Info().
		Str("world", world).
		Str("npc", npc).
		Str("section", section).
		Msg("update_npc")
	if n.slow != nil {
		_ = n.slow.Write("tool.update_npc", "", map[string]any{
			"world":   world,
			"npc":     npc,
			"section": section,
		})
	}
	return nil
}

// SearchResult is the compact view returned by Search.
type SearchResult struct {
	DisplayName   string
	Slug          string
	Temperament   string
	CurrentStatus string
	Source        string
}

// Search resolves a free-form query against the world's
// NPC registry and returns a compact description.
func (n *NPC) Search(world, query string) (*SearchResult, error) {
	defer func() {
		_ = recover()
	}()
	r := n.loadRegistry(world)
	entry, ok := r.Lookup(query)
	if !ok {
		return nil, ErrNPCNotFound
	}
	profile, err := n.repos.NPCProfile.Load(world, entry.Slug)
	if err != nil {
		return nil, ErrNPCNotFound
	}
	return &SearchResult{
		DisplayName:   entry.DisplayName,
		Slug:          entry.Slug,
		Temperament:   profile.Temperament,
		CurrentStatus: profile.CurrentStatus,
		Source:        "yaml",
	}, nil
}

// resolveSlug turns the model's NPC reference (display
// name, nickname, or slug) into the on-disk slug via the
// registry. Returns ErrNPCNotFound when the registry has
// no match — the GM surfaces this to the model as a
// prompt to call create_npc first.
//
// The model occasionally writes the slug directly
// ("naruto_uzumaki"); the worldregistry.Lookup accepts
// that case too.
func (n *NPC) resolveSlug(world, name string) (string, error) {
	r := n.loadRegistry(world)
	entry, ok := r.Lookup(name)
	if !ok {
		return "", ErrNPCNotFound
	}
	return entry.Slug, nil
}

// splitRelationText splits "Target: note" on the
// first colon. Used by Create to parse legacy
// free-text relations.
func splitRelationText(s string) (string, string) {
	for i, r := range s {
		if r == ':' {
			return strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+1:])
		}
	}
	return strings.TrimSpace(s), ""
}
