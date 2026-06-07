package files

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/rs/zerolog"

	"narrative/internal/adapter/storage"
	"narrative/internal/domain"
	"narrative/internal/npcprofile"
	"narrative/internal/usecase/tools"
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
}

func newNPC(fs *storage.FileStore, log zerolog.Logger) *NPC {
	return &NPC{fs: fs, log: log.With().Str("component", "npc").Logger()}
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
		Abilities:   splitList(p.Abilities),
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

// splitList turns a free-form string ("a, b, c") into
// a []string. Used for create_npc's flat-text fields
// (abilities, nicknames) which arrive as a single
// blob from the model. The migration path also
// benefits: a legacy file with "- a\n- b" gets the
// same flat-text treatment.
func splitList(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
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
	name, err := domain.SanitizeName(npc)
	if err != nil {
		return fmt.Errorf("npc name: %w", err)
	}
	rel := "worlds/" + world + "/characters/" + name + ".yaml"
	profile, err := n.loadProfile(rel, name)
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
		return nil
	}
	if err := n.saveProfile(rel, profile); err != nil {
		return err
	}
	n.log.Info().
		Str("world", world).
		Str("npc", name).
		Str("section", kind.CanonicalSectionName()).
		Int("bytes_added", len(appendText)).
		Msg("npc_updated")
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
		{"Способности", p.Abilities},
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

func (n *NPC) appendRegistry(world, display, file string, nicks []string) error {
	rel := "worlds/" + world + "/characters.md"
	cur, _ := n.fs.ReadRaw(rel)
	if cur != "" && !strings.HasSuffix(cur, "\n") {
		cur += "\n"
	}
	nickStr := strings.Join(nicks, ", ")
	cur += "| " + display + " | characters/" + file + " | " + nickStr + " |\n"
	return n.fs.WriteRawAtomic(rel, cur)
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
	name, err := domain.SanitizeName(npc)
	if err != nil {
		return "", fmt.Errorf("npc name: %w", err)
	}
	rel := "worlds/" + world + "/characters/" + name + ".yaml"
	profile, err := n.loadProfile(rel, name)
	if err != nil {
		return "", err
	}
	return profile.BuildMarkdown(), nil
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
