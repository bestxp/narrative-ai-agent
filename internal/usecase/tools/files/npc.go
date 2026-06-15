package files

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/rs/zerolog"

	"github.com/bestxp/narrative-ai-agent/internal/adapter/storage"
	"github.com/bestxp/narrative-ai-agent/internal/domain"
	"github.com/bestxp/narrative-ai-agent/internal/npcprofile"
	"github.com/bestxp/narrative-ai-agent/internal/slowlog"
	"github.com/bestxp/narrative-ai-agent/internal/usecase/tools"
	"github.com/bestxp/narrative-ai-agent/internal/worldregistry"
)

// NPC is the file-backed implementation of tools.NPCTool:
// create on first appearance, plus the knowledge-isolation
// helper that filters a candidate reply down to what the
// active NPC may say.
//
// Storage shape: the on-disk file is YAML (the canonical
// representation in internal/npcprofile), stored at
// worlds/<world>/characters/<slug>.yaml. The model and
// the dispatcher never see YAML — Load returns the
// rendered markdown (Profile.BuildMarkdown) so existing
// prompts, parser markers, and operator reads do not
// need to change. The flip side: every Load-then-Save
// round-trip goes through Profile, so the file is
// normalised on every update (no stale line layouts).
type NPC struct {
	fs  *storage.FileStore
	log zerolog.Logger
	// slow is the audit log; nil-safe (Write checks nil).
	// Wired at construction in NewFileToolset; tests pass
	// slowlog.Discard(). Emits `tool.update_npc` events so
	// the operator can correlate an LLM-driven `update_npc`
	// call with the on-disk change. Without this, the
	// only trace is the `npc_updated` zerolog Info line —
	// which is fine for human eyeballs but useless for
	// the regression suite (no structured `kind=...`
	// prefix to grep against).
	slow *slowlog.Logger
}

func newNPC(fs *storage.FileStore, log zerolog.Logger, slow *slowlog.Logger) *NPC {
	return &NPC{
		fs:   fs,
		log:  log.With().Str("component", "npc").Logger(),
		slow: slow,
	}
}

// ErrNPCExists is returned when Create is called for an NPC
// that already has a file.
var ErrNPCExists = errors.New("npc file already exists")

// ErrNPCNotFound is returned when UpdateNPC is called for an
// NPC whose profile does not exist on disk. The model must
// call Create first; Load("") reports the same error so the
// GM can detect the gap before dispatching UpdateNPC.
var ErrNPCNotFound = errors.New("npc profile not found; call create_npc first")

// datedEventRe matches "2026-06-01: ..." style dated events
// that CompactNPCBody strips from NPC files. Kept for
// backward compatibility — the new summarizer path does
// not need it, but the legacy run_maintenance tool may
// still be called and the data is here.
var datedEventRe = regexp.MustCompile(`(?m)^-\s+\d{4}-\d{2}-\d{2}.*$`)

// loadProfile reads the on-disk file and returns the
// canonical Profile. If the file is missing or empty,
// returns ErrNPCFound. If the file exists but is the
// legacy markdown shape, MigrateFromMarkdown lifts it
// into a Profile on the fly (the file is rewritten in
// YAML on the next Save).
func (n *NPC) loadProfile(rel, name string) (npcprofile.Profile, error) {
	raw, err := n.fs.ReadRaw(rel)
	if err != nil || strings.TrimSpace(raw) == "" {
		return npcprofile.Profile{}, ErrNPCNotFound
	}
	// Try YAML first (the canonical shape). If the
	// unmarshal fails AND the body looks like markdown
	// (starts with "# " or "## "), fall back to the
	// legacy migrator. A truly corrupt file (neither
	// YAML nor markdown) bubbles up as a hard error so
	// the operator notices.
	p, yamlErr := npcprofile.Load(raw)
	if yamlErr == nil && p.DisplayName != "" {
		return p, nil
	}
	if looksLikeMarkdown(raw) {
		n.log.Info().Str("file", rel).Msg("npc: migrating legacy markdown to YAML")
		return npcprofile.MigrateFromMarkdown(raw, name)
	}
	if yamlErr != nil {
		return npcprofile.Profile{}, fmt.Errorf("npc %q: %w", name, yamlErr)
	}
	return npcprofile.Profile{}, fmt.Errorf("npc %q: file exists but is empty/invalid", name)
}

// looksLikeMarkdown returns true when the body starts
// with a "# " or "## " heading. We use this to decide
// whether to fall back to the legacy markdown migrator
// when YAML unmarshalling fails (a YAML file with no
// required fields will look like an empty struct, not
// a parse error — distinguishing the two is what this
// helper is for).
func looksLikeMarkdown(s string) bool {
	t := strings.TrimSpace(s)
	return strings.HasPrefix(t, "# ") || strings.HasPrefix(t, "## ")
}

// saveProfile serialises a Profile back to disk in the
// canonical YAML form. The directory is ensured to
// exist so the caller does not have to.
func (n *NPC) saveProfile(rel string, p npcprofile.Profile) error {
	body, err := p.Save()
	if err != nil {
		return err
	}
	return n.fs.WriteRawAtomic(rel, body)
}

// Create writes a new NPC profile to disk. The body is
// rendered in YAML form (Profile.Save), the markdown
// view (Profile.BuildMarkdown) is what Load returns
// when callers want the human-readable shape.
func (n *NPC) Create(world string, p tools.NPCProfile) error {
	name, err := domain.SanitizeName(p.File)
	if err != nil {
		return err
	}
	rel := "worlds/" + world + "/characters/" + name + ".yaml"
	if n.fs.Exists(rel) {
		return ErrNPCExists
	}
	if err := n.fs.EnsureDir("worlds/" + world + "/characters"); err != nil {
		return err
	}
	profile := npcprofile.Profile{
		DisplayName: p.DisplayName,
		FileSlug:    name,
		Temperament: p.Temperament,
		RelationsGG: p.Relations,
		Abilities:   p.Abilities,
		Nicknames:   p.Nicknames,
	}
	if err := n.saveProfile(rel, profile); err != nil {
		return err
	}
	if err := n.appendRegistry(world, p.DisplayName, name, p.Nicknames); err != nil {
		return err
	}
	n.log.Info().Str("world", world).Str("npc", name).Msg("npc_created")
	return nil
}

// splitList turns a free-form string into a []string.
// Used for create_npc's flat-text fields (abilities, nicknames)
// which arrive as a single blob from the model.
//
// Heuristic:
//   - Multi-line text (contains "\n") → treat as bullet list,
//     split by lines and strip "- " / "* " / "• " prefixes.
//   - Single line with commas → only split if each part is short
//     (≤ 60 chars). If any part is long, treat the whole string
//     as a single prose entry (e.g. "Мастер обращения с оружием
//     — кунаями, сюрикенами, цепями..." should stay intact).
func splitList(s string) []string {
	if s == "" {
		return nil
	}
	// 1. Multi-line: bullet list
	lines := strings.Split(s, "\n")
	if len(lines) > 1 {
		out := make([]string, 0, len(lines))
		for _, l := range lines {
			l = strings.TrimSpace(l)
			l = strings.TrimPrefix(l, "- ")
			l = strings.TrimPrefix(l, "* ")
			l = strings.TrimPrefix(l, "• ")
			if l != "" {
				out = append(out, l)
			}
		}
		return out
	}
	// 2. Single line with commas
	parts := strings.Split(s, ",")
	if len(parts) > 1 {
		// Heuristic: if any part is > 60 chars, it's prose, not a list
		for _, p := range parts {
			if len(strings.TrimSpace(p)) > 60 {
				return []string{strings.TrimSpace(s)}
			}
		}
	}
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// loadRegistry reads worlds/<w>/characters.yaml
// (with legacy characters.md fallback inside
// worldregistry.Load). Cheap: a few hundred bytes
// of YAML at most, parsed on every call. The
// dispatcher calls UpdateNPC and Load many times
// per turn, so the cost matters; the registry is
// small enough that re-parsing is fine. We
// deliberately do NOT cache across calls because
// Create mutates the registry and a stale cache
// would route subsequent UpdateNPCs to a deleted
// file.
func (n *NPC) loadRegistry(world string) (*worldregistry.Registry, error) {
	return worldregistry.Load(n.fs, world)
}

// findNPCFile resolves a display name to the
// on-disk YAML file. The lookup is
// registry-driven: display_name → slug, NOT
// transliteration-driven. "Хината" matches
// "Хината Хьюга" via the registry, not via
// "khinata". The operator's hand-picked slugs
// (hinata, anbu_dog, iruka, kurotsuba) are
// preserved exactly as the legacy files carry
// them.
//
// Lookup strategy:
//  1. Try the registry. If the world has a
//     characters.yaml (or characters.md bootstrap)
//     and the display name matches one of the
//     entries, return the registry's slug.
//  2. If the registry has NO entry for the name
//     (regardless of why — empty registry,
//     missing file, no match) fall through to a
//     directory scan of worlds/<w>/characters/.
//     The scan reads each profile's display_name
//     and applies the same substring heuristic the
//     registry would have. This is the survival
//     path for operator setups that have not been
//     migrated to characters.yaml yet.
//
// Returns (rel, slug, true) on a hit. On no hit
// returns ("", "", false) — the caller maps that
// to ErrNPCNotFound and the GM surfaces "create_npc
// first" to the model.
func (n *NPC) findNPCFile(world, displayName string) (string, string, bool) {
	if reg, err := n.loadRegistry(world); err == nil {
		if e, ok := reg.Lookup(displayName); ok {
			rel := "worlds/" + world + "/characters/" + e.Slug + ".yaml"
			if n.fs.Exists(rel) {
				return rel, e.Slug, true
			}
			// Registry points at a file that is
			// not on disk. Do NOT fall through to
			// the scan — the operator may be
			// mid-rename, and UpdateNPC must not
			// silently create a profile. We
			// return a hard "not found" so the
			// GM surfaces a clear error.
			return "", "", false
		}
	}
	return n.findNPCFileFallback(world, displayName)
}

// findNPCFileFallback is the last-resort path used
// when characters.yaml is missing or unreadable.
// It scans worlds/<w>/characters/*.yaml and tries
// to find a matching display_name inside the
// profile body. This is what kept the bot working
// before the registry existed; it survives a
// deleted/empty characters.yaml but loses the
// nickname + substring matching the registry
// offers.
func (n *NPC) findNPCFileFallback(world, displayName string) (string, string, bool) {
	dir := "worlds/" + world + "/characters"
	entries, err := n.fs.ListChildren(dir)
	if err != nil {
		return "", "", false
	}
	want := strings.ToLower(strings.TrimSpace(displayName))
	if want == "" {
		return "", "", false
	}
	for _, fn := range entries {
		if !strings.HasSuffix(fn, ".yaml") {
			continue
		}
		rel := dir + "/" + fn
		raw, _ := n.fs.ReadRaw(rel)
		slug := strings.TrimSuffix(fn, ".yaml")
		if p, err := npcprofile.Load(raw); err == nil && p.DisplayName != "" {
			if strings.EqualFold(strings.TrimSpace(p.DisplayName), want) ||
				strings.Contains(strings.ToLower(p.DisplayName), want) {
				return rel, slug, true
			}
		}
	}
	return "", "", false
}

// UpdateNPC appends fresh facts to an existing NPC's
// profile. The `section` argument is the canonical
// section name (case-insensitive, English aliases
// accepted); the text is appended with dedup. The
// "## Последнее обновление" section is special: its
// body is REPLACED rather than appended, so a reader
// can always see the freshest line at the bottom.
//
// Use UpdateNPC for:
//   - new abilities an NPC demonstrated this scene
//   - a relationship shift ("статус: друг → наставник")
//   - a fact the player revealed to the NPC
//   - a piece of critical knowledge the NPC just learned
//   - a current status change ("локация: Коноха → миссия в Стране Волн")
//
// The model's contract: write SHORT, FACTUAL lines —
// not summaries. "Стал читать мысли после Дня 5" not
// "произошло много всего". Each call adds ONE fact.
func (n *NPC) UpdateNPC(world, npc, section, appendText string) error {
	rel, slug, ok := n.findNPCFile(world, npc)
	if !ok {
		return ErrNPCNotFound
	}
	profile, err := n.loadProfile(rel, npc)
	if err != nil {
		return err
	}
	kind := npcprofile.MatchSection(section)
	if kind == npcprofile.SectionUnknown {
		return fmt.Errorf("update_npc: unknown section %q (allowed: %v)",
			section, npcAllowedSections())
	}
	if strings.TrimSpace(appendText) == "" {
		return nil
	}
	changed := profile.UpdateSection(kind, appendText)
	if !changed {
		// Dedup hit — nothing to write but the
		// call is still a no-op success. The
		// caller (the GM) does not need a
		// warning; the slowlog already shows
		// how many turns produced an effective
		// change.
		// Still emit a `tool.update_npc` slowlog
		// event with `changed: false` so an
		// operator reading the trace can see the
		// LLM *attempted* an update, even if the
		// summarizer-side dedupe rejected it.
		if n.slow != nil {
			_ = n.slow.Write("tool.update_npc", "", map[string]any{
				"world":    world,
				"npc":      slug,
				"section":  kind.CanonicalSectionName(),
				"changed":  false,
				"bytes_in": len(appendText),
				"path":     rel,
			})
		}
		return nil
	}
	if err := n.saveProfile(rel, profile); err != nil {
		return err
	}
	n.log.Info().
		Str("world", world).
		Str("npc", slug).
		Str("section", kind.CanonicalSectionName()).
		Int("bytes_added", len(appendText)).
		Msg("npc_updated")
	if n.slow != nil {
		_ = n.slow.Write("tool.update_npc", "", map[string]any{
			"world":       world,
			"npc":         slug,
			"section":     kind.CanonicalSectionName(),
			"changed":     true,
			"bytes_added": len(appendText),
			"path":        rel,
		})
	}
	return nil
}

// npcAllowedSections is the human-readable list for
// error messages. The model is told these in
// narrative.md (see the КОНТЕКСТНЫЕ ДИРЕКТИВЫ section);
// the error string here is the fallback when a section
// name slips through the parser.
func npcAllowedSections() []string {
	return []string{
		"Темперамент", "Отношения с ГГ",
		"Отношения с другими NPC", "Способности",
		"Личная память/факты", "Текущий статус",
		"Критические знания", "Никнеймы",
		"Последнее обновление",
	}
}

// BuildNPCMarkdown renders a fresh NPC profile from the
// legacy NPCProfile struct. The canonical render path
// for files on disk is npcprofile.Profile (YAML); this
// function is the legacy markdown renderer kept
// verbatim for backward compatibility with tests and
// external callers that still hand us a tools.NPCProfile.
// The block layout — nicknames at the top, the
// "## Отношения с другими NPC" placeholder, and the
// "## Последнее обновление" footer at the bottom —
// matches the pre-YAML shape exactly so the model and
// the operator see the same file layout.
func BuildNPCMarkdown(p tools.NPCProfile) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n", strings.TrimSpace(p.DisplayName))
	if len(p.Nicknames) > 0 {
		fmt.Fprintf(&b, "_Прозвища: %s_\n", strings.Join(p.Nicknames, ", "))
	}
	sections := []struct {
		name    string
		content string
	}{
		{"Темперамент", p.Temperament},
		{"Отношения с ГГ", p.Relations},
		{"Отношения с другими NPC", ""}, // placeholder, model fills via update_npc
		{"Способности", strings.Join(p.Abilities, "\n")},
		{"Личная память/факты", p.PersonalMemory},
		{"Текущий статус", p.CurrentStatus},
		{"Критические знания", p.CriticalKnowledge},
	}
	for _, s := range sections {
		if strings.TrimSpace(s.content) == "" {
			continue
		}
		fmt.Fprintf(&b, "\n## %s\n%s\n", s.name, strings.TrimSpace(s.content))
	}
	// "Отношения с другими NPC" placeholder when there are
	// no other sections — gives the model a place to start
	// writing on the first update_npc call.
	b.WriteString("\n## Отношения с другими NPC\n_(допиши через update_npc)_\n")
	// Last update — always rendered so future UpdateNPC
	// calls have a stable landing pad.
	if strings.TrimSpace(p.LastUpdate) != "" {
		fmt.Fprintf(&b, "\n## Последнее обновление\n%s\n", strings.TrimSpace(p.LastUpdate))
	} else {
		b.WriteString("\n## Последнее обновление\n_(пусто)_\n")
	}
	return b.String()
}

// appendRegistry adds a freshly-created NPC to
// worlds/<w>/characters.yaml. The entry's slug is
// the on-disk file name; the registry is the
// canonical index of who exists in this world.
// On any failure to persist the registry we still
// return nil — the file itself was written, and
// the next call to loadRegistry will pick up the
// entry via the directory-scan fallback. Better
// to have a working profile with a stale registry
// than no profile at all.
func (n *NPC) appendRegistry(world, display, file string, nicks []string) error {
	reg, err := worldregistry.Load(n.fs, world)
	if err != nil {
		n.log.Warn().Err(err).Str("world", world).Msg("registry load failed during append")
		return nil
	}
	// Idempotency: if the slug is already there
	// (operator hand-edited the file, or this
	// is a retry of a failed Create) leave it
	// alone and just rewrite the YAML with the
	// latest entry set.
	for _, e := range reg.All() {
		if e.Slug == file {
			break
		}
	}
	if err := reg.Add(worldregistry.Entry{
		Slug:        file,
		DisplayName: display,
		Nicknames:   nicks,
	}); err != nil {
		// Duplicate slug (already in the
		// registry) is not a fatal error: the
		// profile itself was just created, and
		// a second registry write would lose
		// nickname edits. Log and move on.
		n.log.Info().Err(err).Str("world", world).Str("slug", file).Msg("registry add skipped")
		return nil
	}
	body, err := reg.Save()
	if err != nil {
		n.log.Warn().Err(err).Str("world", world).Msg("registry marshal failed during append")
		return nil
	}
	if err := n.fs.WriteRawAtomic("worlds/"+world+"/characters.yaml", body); err != nil {
		n.log.Warn().Err(err).Str("world", world).Msg("registry write failed during append")
	}
	return nil
}

// Load returns the NPC's profile rendered as markdown
// (the same shape the model and the parser expect).
// Returns ErrNPCNotFound when the file is missing or
// empty so callers (the GM's missedToolGuard, /me,
// and UpdateNPC) can distinguish "no profile yet" from
// "profile exists but is blank".
//
// The internal storage is YAML; the markdown view is
// regenerated on every Load via Profile.BuildMarkdown
// so callers always see the canonical layout. The
// model never sees the YAML keys.
func (n *NPC) Load(world, npc string) (string, error) {
	rel, _, ok := n.findNPCFile(world, npc)
	if !ok {
		return "", ErrNPCNotFound
	}
	profile, err := n.loadProfile(rel, npc)
	if err != nil {
		return "", err
	}
	return profile.BuildMarkdown(), nil
}

// LoadLOD returns the NPC profile rendered at the
// requested level of detail. The caller is the LOD
// policy (loadActiveNPCs in gm.go); the file backend
// just reads once and re-renders.
//
// The three LODs map to the three renderers in
// internal/npcprofile:
//
//   - tools.LODFull    → BuildMarkdown (everything)
//   - tools.LODCompact → BuildCompact (no big arrays)
//   - tools.LODOneLine → BuildOneLine (one line)
//
// An unknown LOD (e.g. an out-of-range int from a
// future caller) falls back to Full — safer than
// dropping the NPC from the world block silently.
func (n *NPC) LoadLOD(world, npc string, lod tools.NPCLOD) (string, error) {
	rel, _, ok := n.findNPCFile(world, npc)
	if !ok {
		return "", ErrNPCNotFound
	}
	profile, err := n.loadProfile(rel, npc)
	if err != nil {
		return "", err
	}
	switch lod {
	case tools.LODCompact:
		return profile.BuildCompact(), nil
	case tools.LODOneLine:
		return profile.BuildOneLine(), nil
	case tools.LODFull:
		return profile.BuildMarkdown(), nil
	default:
		return profile.BuildMarkdown(), nil
	}
}

// SearchResult is the compact view the model sees for
// a search_npc hit. It is intentionally short (1-3
// lines) so the tool result does not blow the messages
// cache — the model only needs enough to confirm "yes
// this is who I was thinking of" and decide whether
// to add the NPC to the active roster via update_state.
type SearchResult struct {
	DisplayName string `json:"display_name"`
	Slug        string `json:"slug"`
	// Temperament is a one-sentence description of the
	// NPC's baseline personality.
	Temperament string `json:"temperament,omitempty"`
	// CurrentStatus is a one-sentence "where they are /
	// what they're doing right now" snapshot.
	CurrentStatus string `json:"current_status,omitempty"`
	// Source tells the model which registry field matched
	// ("display_name", "slug", "nickname", "substring")
	// — useful for the operator in slowlog, less so for
	// the model.
	Source string `json:"source"`
}

// Search resolves a free-form query against the world's
// NPC registry. The result is a compact description —
// not the full YAML. The model should only call this
// when it needs an NPC that is not already in the active
// roster (i.e. search_npc is a fallback, not a
// replacement for the always-on roster).
//
// Match priority follows worldregistry.Lookup: exact
// (slug / display_name / nickname), then unambiguous
// substring. Ambiguous substring matches return an
// error so the model can disambiguate before
// retrieving any profile.
//
// Lookup path: try the registry (characters.yaml)
// first — it carries slug + display + nicknames in one
// map and resolves substring matches. If the registry
// is empty / missing, fall through to the directory-scan
// fallback (findNPCFileFallback) which inspects each
// profile on disk. This mirrors the resolution path
// used by Load / UpdateNPC, so an operator without
// characters.yaml still gets correct results.
func (n *NPC) Search(world, query string) (*SearchResult, error) {
	// Step 1: try the registry. Cheap map lookup;
	// handles the common case (operator maintains
	// characters.yaml).
	if reg, err := n.loadRegistry(world); err == nil {
		if entry, ok := reg.Lookup(query); ok {
			rel := "worlds/" + world + "/characters/" + entry.Slug + ".yaml"
			profile, err := n.loadProfile(rel, entry.Slug)
			if err != nil {
				return nil, fmt.Errorf("search_npc: registry hit %q but file load failed: %w", entry.Slug, err)
			}
			return n.searchResultFromProfile(profile, query), nil
		}
	}
	// Step 2: registry miss — directory scan. Resolves
	// a display_name match when the registry is empty
	// or stale. findNPCFile is the same helper Load and
	// UpdateNPC use, so all three paths agree on
	// resolution rules.
	rel, slug, ok := n.findNPCFile(world, query)
	if !ok {
		return nil, ErrNPCNotFound
	}
	profile, err := n.loadProfile(rel, slug)
	if err != nil {
		return nil, fmt.Errorf("search_npc: directory hit %q but file load failed: %w", slug, err)
	}
	return n.searchResultFromProfile(profile, query), nil
}

// searchResultFromProfile is the common path that
// turns a parsed npcprofile.Profile into the compact
// SearchResult. Source is best-effort — the registry
// path would tell us "matched on nickname" etc., but
// the directory-scan fallback only knows "matched
// somewhere on display_name". Operators can still get
// the full story from the slowlog event.
func (n *NPC) searchResultFromProfile(profile npcprofile.Profile, query string) *SearchResult {
	res := &SearchResult{
		DisplayName:   profile.DisplayName,
		Slug:          profile.FileSlug,
		Temperament:   profile.Temperament,
		CurrentStatus: profile.CurrentStatus,
	}
	if strings.EqualFold(strings.TrimSpace(query), profile.FileSlug) {
		res.Source = "slug"
	} else {
		res.Source = "display_name_or_nickname"
	}
	return res
}

// CompactNPCBody is the legacy strip implementation. It
// remains in the file because the run_maintenance tool
// path still calls it; the new summarizer path (LLM-
// driven) will replace it once that work lands. The
// function is pure (no I/O) so callers control when
// to read / write.
func CompactNPCBody(body string) string {
	if body == "" {
		return body
	}
	lines := strings.Split(body, "\n")
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		if datedEventRe.MatchString(ln) {
			continue
		}
		out = append(out, ln)
	}
	return strings.Join(out, "\n")
}

// FilterKnowledge strips any line from `candidate` whose marker
// is NOT present in `allowed`. The marker convention is
// `<!NPC:marker!>` at line end. This is the runtime helper for
// "info isolation" — the GM writes a candidate reply with all
// knowledge it COULD say, and the manager drops anything the
// NPC has not earned.
func FilterKnowledge(candidate, allowed string) string {
	if candidate == "" {
		return ""
	}
	var b strings.Builder
	for _, line := range strings.Split(candidate, "\n") {
		markers := extractMarkers(line)
		if len(markers) == 0 {
			b.WriteString(line)
			b.WriteByte('\n')
			continue
		}
		ok := true
		for _, mk := range markers {
			if !strings.Contains(allowed, mk) {
				ok = false
				break
			}
		}
		if ok {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func extractMarkers(line string) []string {
	var out []string
	low := line
	for {
		start := strings.Index(low, "<!NPC:")
		if start < 0 {
			break
		}
		rest := low[start+len("<!NPC:"):]
		end := strings.Index(rest, "!>")
		if end < 0 {
			break
		}
		out = append(out, "<!NPC:"+rest[:end]+"!>")
		low = rest[end+2:]
	}
	return out
}
