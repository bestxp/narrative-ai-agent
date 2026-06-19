package files

import (
	"errors"
	"regexp"
	"strings"

	"github.com/rs/zerolog"

	"github.com/bestxp/narrative-ai-agent/internal/domain"
	"github.com/bestxp/narrative-ai-agent/internal/npcprofile"
	"github.com/bestxp/narrative-ai-agent/internal/repository/api"
	"github.com/bestxp/narrative-ai-agent/internal/slowlog"
	"github.com/bestxp/narrative-ai-agent/internal/usecase/tools"
	"github.com/bestxp/narrative-ai-agent/internal/worldregistry"
)

// NPC is the repository-backed implementation of
// tools.NPCTool: create on first appearance, plus
// the knowledge-isolation helper that filters a
// candidate reply down to what the active NPC may say.
//
// All persistent reads and writes go through
// *api.Repositories (NPCProfileRepository for the
// per-NPC YAML, NPCRegistryRepository for the
// worlds/<w>/characters.md roster table).
type NPC struct {
	repos *api.Repositories
	log   zerolog.Logger
	slow  *slowlog.Logger
}

func newNPC(log zerolog.Logger, slow *slowlog.Logger, repos *api.Repositories) *NPC {
	return &NPC{
		repos: repos,
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

// Create writes a new NPC profile to the world's
// characters directory via repos.NPCProfile + the
// registry. Returns ErrNPCExists if the slug is
// already taken.
func (n *NPC) Create(world string, p tools.NPCProfile) error {
	slug := sanitizeName(p.DisplayName)
	if slug == "" {
		return errors.New("npc create: empty display name")
	}

	// Check for existing file.
	_, err := n.repos.NPCProfile.Load(world, slug)
	if err == nil {
		return ErrNPCExists
	}
	if err != npcprofile.ErrNotFound {
		return err
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
		for _, line := range strings.Split(p.Relations, "\n") {
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
		for _, line := range strings.Split(p.PersonalMemory, "\n") {
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
		for _, line := range strings.Split(p.CriticalKnowledge, "\n") {
			if t := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "- ")); t != "" {
				profile.CriticalKnowledge = append(profile.CriticalKnowledge, t)
			}
		}
	}
	profile.LastUpdate = strings.TrimSpace(p.LastUpdate)

	if err := n.repos.NPCProfile.Save(world, slug, profile); err != nil {
		return err
	}

	// Append to the registry.
	if err := n.repos.NPCRegistry.AppendEntry(world, slug, profile.DisplayName, profile.Nicknames); err != nil {
		n.log.Warn().Err(err).Str("slug", slug).Msg("create_npc: registry append failed")
	}

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
func (n *NPC) LoadLOD(world, npc string, lod tools.NPCLOD) (string, error) {
	slug := npc
	// If the caller passed a display name, resolve to slug.
	if !looksLikeSlug(npc) {
		resolved, ok := n.findNPCSlug(world, npc)
		if !ok {
			return "", ErrNPCNotFound
		}
		slug = resolved
	}
	profile, err := n.repos.NPCProfile.Load(world, slug)
	if err != nil {
		if err == npcprofile.ErrNotFound {
			return "", ErrNPCNotFound
		}
		return "", err
	}
	switch lod {
	case tools.LODFull:
		body, err := profile.BuildMarkdown()
		if err != nil {
			return "", err
		}
		return body, nil
	case tools.LODCompact:
		return profile.BuildCompact(), nil
	case tools.LODOneLine:
		return profile.BuildOneLine(), nil
	}
	body, err := profile.BuildMarkdown()
	if err != nil {
		return "", err
	}
	return body, nil
}

// UpdateNPC appends fresh facts to an existing NPC
// profile via repos.NPCProfile.UpdateSection.
func (n *NPC) UpdateNPC(world, npc, section, appendText string) error {
	slug := npc
	if !looksLikeSlug(npc) {
		resolved, ok := n.findNPCSlug(world, npc)
		if !ok {
			return ErrNPCNotFound
		}
		slug = resolved
	}
	ok, err := n.repos.NPCProfile.UpdateSection(world, slug, section, appendText)
	if err != nil {
		return err
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
func (n *NPC) Search(world, query string) (result *SearchResult, err error) {
	defer func() {
		if r := recover(); r != nil {
			result = nil
			err = nil
		}
	}()
	slug, display, ok := n.findNPCSlugByQuery(world, query)
	if !ok {
		return nil, nil
	}
	profile, err := n.repos.NPCProfile.Load(world, slug)
	if err != nil {
		return nil, ErrNPCNotFound
	}
	return &SearchResult{
		DisplayName:   display,
		Slug:          slug,
		Temperament:   profile.Temperament,
		CurrentStatus: profile.CurrentStatus,
		Source:        "yaml",
	}, nil
}

// findNPCSlug resolves a display name to the file slug
// via the NPC registry. Returns ("", "", false) when
// the name is not found.
func (n *NPC) findNPCSlug(world, displayName string) (string, bool) {
	slug, _, ok := n.findNPCSlugByQuery(world, displayName)
	return slug, ok
}

// findNPCSlugByQuery walks the registry table looking
// for a row whose display name or nickname matches the
// query (case-insensitive).
func (n *NPC) findNPCSlugByQuery(world, query string) (slug, display string, ok bool) {
	body, err := n.repos.NPCRegistry.Load(world)
	if err != nil || body == "" {
		return "", "", false
	}
	query = strings.ToLower(strings.TrimSpace(query))
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "|") {
			continue
		}
		dn, sl, nicks, found := parseRegistryRow(line)
		if !found {
			continue
		}
		if strings.ToLower(dn) == query {
			return sl, dn, true
		}
		for _, nick := range nicks {
			if strings.ToLower(nick) == query {
				return sl, dn, true
			}
		}
	}
	return "", "", false
}

// parseRegistryRow parses one row of the NPC registry
// markdown table. Returns (displayName, slug, nicknames, ok).
func parseRegistryRow(line string) (displayName, slug string, nicknames []string, ok bool) {
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
		for _, nck := range strings.Split(nickCol, ",") {
			if v := strings.TrimSpace(nck); v != "" {
				nicknames = append(nicknames, v)
			}
		}
	}
	return displayName, slug, nicknames, true
}

// registryRowRe matches the per-NPC row written by
// the registry's AppendEntry.
var registryRowRe = regexp.MustCompile(`^\|\s*([^|]+?)\s*\|\s*([^|]+?)\s*\|\s*([^|]*?)\s*\|$`)

// looksLikeSlug returns true if the string looks like
// a file slug (lowercase, hyphens, no spaces) rather
// than a display name (mixed case, spaces).
func looksLikeSlug(s string) bool {
	if s == "" {
		return false
	}
	hasSpace := strings.ContainsAny(s, " \t")
	hasUpper := s != strings.ToLower(s)
	return !hasSpace && !hasUpper
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

// Ensure the NPC struct satisfies the NPCRegistry
// consumer contract (registry rows are parsed via
// parseRegistryRow, which mirrors the yaml
// NPCRegistryYaml.AppendEntry format).
var _ = worldregistry.Registry{}

// Reference unused imports that are part of the public
// interface but used only in edge-case paths.
var _ = domain.Transliterate
