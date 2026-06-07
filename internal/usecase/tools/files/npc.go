package files

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/rs/zerolog"

	"narrative/internal/adapter/storage"
	"narrative/internal/domain"
	"narrative/internal/usecase/tools"
)

// NPC is the file-backed implementation of tools.NPCTool:
// create on first appearance, plus the knowledge-isolation
// helper that filters a candidate reply down to what the
// active NPC may say.
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
// that CompactNPCBody strips from NPC files. The 4-digit year
// keeps false positives low (a regular line rarely starts with
// "YYYY-MM-DD:").
var datedEventRe = regexp.MustCompile(`(?m)^-\s+\d{4}-\d{2}-\d{2}.*$`)

// Create writes a new NPC profile to disk. The body is
// rendered in the order the canonical section names
// appear in canonicalNPCSections; missing sections are
// dropped. The fixed order matters: a future UpdateNPC
// call can find the "## Способности" header by line position
// without a parser.
func (n *NPC) Create(world string, p tools.NPCProfile) error {
	name, err := domain.SanitizeName(p.File)
	if err != nil {
		return err
	}
	rel := "worlds/" + world + "/characters/" + name + ".md"
	if n.fs.Exists(rel) {
		return ErrNPCExists
	}
	if err := n.fs.EnsureDir("worlds/" + world + "/characters"); err != nil {
		return err
	}
	body := BuildNPCMarkdown(p)
	if err := n.fs.WriteRawAtomic(rel, body); err != nil {
		return err
	}
	if err := n.appendRegistry(world, p.DisplayName, name, p.Nicknames); err != nil {
		return err
	}
	n.log.Info().Str("world", world).Str("npc", name).Msg("npc_created")
	return nil
}

// UpdateNPC appends fresh facts to an existing NPC's profile.
// The `section` argument is one of the canonical section
// names (case-insensitive); the section is created on first
// use. The "## Последнее обновление" section is special:
// instead of appending, its body is REPLACED with the
// timestamp + the new fact, so a reader can always see the
// freshest line at the bottom of the file.
//
// Use UpdateNPC for:
//   - new abilities an NPC demonstrated this scene
//   - a relationship shift ("статус: друг → наставник")
//   - a fact the player revealed to the NPC
//   - a piece of critical knowledge the NPC just learned
//   - a current status change ("локация: Коноха → миссия в Стране Волн")
//
// The model's contract: write SHORT, FACTUAL lines — not
// summaries. "Стал читать мысли после Дня 5" not
// "произошло много всего". Each call adds ONE fact, not a
// paragraph. The re-render keeps the file readable instead
// of turning into a stream-of-consciousness diary.
func (n *NPC) UpdateNPC(world, npc, section, appendText string) error {
	name, err := domain.SanitizeName(npc)
	if err != nil {
		return fmt.Errorf("npc name: %w", err)
	}
	rel := "worlds/" + world + "/characters/" + name + ".md"
	body, err := n.fs.ReadRaw(rel)
	// ReadRaw returns "" + nil when the file is missing;
	// treat that the same as a non-existent profile so
	// UpdateNPC is a strict superset of Create, not a
	// silent "create on update" path.
	if err != nil || body == "" {
		return ErrNPCNotFound
	}
	canonical := canonicalSectionFor(section)
	if canonical == "" {
		return fmt.Errorf("update_npc: unknown section %q (allowed: %v)", section, canonicalNPCSectionNames())
	}
	if strings.TrimSpace(appendText) == "" {
		return nil
	}
	updated, err := upsertNPCSection(body, canonical, appendText)
	if err != nil {
		return err
	}
	if err := n.fs.WriteRawAtomic(rel, updated); err != nil {
		return err
	}
	n.log.Info().
		Str("world", world).
		Str("npc", name).
		Str("section", canonical).
		Int("bytes_added", len(appendText)).
		Msg("npc_updated")
	return nil
}

// canonicalNPCSections is the ordered list of section
// names rendered by BuildNPCMarkdown and accepted by
// UpdateNPC. The order is the same as the rendered file
// so a reader can grep "## " and read top-to-bottom
// without surprises. New sections go at the end so
// existing reader muscle-memory is preserved.
var canonicalNPCSections = []string{
	"Темперамент",
	"Отношения с ГГ",
	"Отношения с другими NPC",
	"Способности",
	"Личная память/факты",
	"Текущий статус",
	"Критические знания",
	"Последнее обновление",
}

// canonicalNPCSectionNames returns the human-readable list
// for error messages.
func canonicalNPCSectionNames() []string {
	out := make([]string, len(canonicalNPCSections))
	for i, s := range canonicalNPCSections {
		out[i] = "## " + s
	}
	return out
}

// canonicalSectionFor normalises a user-supplied section
// name to the canonical spelling. Accepts any case
// ("способности", "СПОСОБНОСТИ", "Способности") and matches
// against the canonical list. Returns "" if the name
// doesn't match any canonical section; UpdateNPC rejects
// unknown sections.
func canonicalSectionFor(raw string) string {
	cleaned := strings.TrimSpace(raw)
	lower := strings.ToLower(cleaned)
	for _, c := range canonicalNPCSections {
		if strings.ToLower(c) == lower {
			return c
		}
	}
	// Allow a few common aliases the model is likely to
	// emit ("abilities", "relations", "status", etc.).
	aliases := map[string]string{
		"temperament":     "Темперамент",
		"personality":     "Темперамент",
		"persona":         "Темперамент",
		"relations":       "Отношения с ГГ",
		"relationships":   "Отношения с ГГ",
		"relation with gg": "Отношения с ГГ",
		"abilities":       "Способности",
		"powers":          "Способности",
		"skills":          "Способности",
		"memory":          "Личная память/факты",
		"personal memory":  "Личная память/факты",
		"facts":           "Личная память/факты",
		"status":          "Текущий статус",
		"current status":  "Текущий статус",
		"knowledge":       "Критические знания",
		"critical":        "Критические знания",
		"update":          "Последнее обновление",
		"last update":     "Последнее обновление",
	}
	if c, ok := aliases[lower]; ok {
		return c
	}
	return ""
}

// indexOfCanonical returns the position of name in
// canonicalNPCSections, or -1 if not present.
func indexOfCanonical(name string) int {
	for i, c := range canonicalNPCSections {
		if c == name {
			return i
		}
	}
	return -1
}

// BuildNPCMarkdown renders a fresh NPC profile from the
// NPCProfile struct. Sections with empty content are
// omitted. The order is the canonicalNPCSections order; the
// "## Последнее обновление" section is the only one that
// always renders, even if empty (with a placeholder so the
// reader knows where fresh facts land).
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

// upsertNPCSection inserts appendText into the canonical
// section of an NPC profile body. If the section header
// is missing, it is appended at the position the canonical
// list says it belongs (so the file remains in canonical
// order after a series of out-of-order updates). The
// "## Последнее обновление" section is special: its body
// is REPLACED rather than appended, so a reader can always
// see the freshest line at the bottom of the file.
func upsertNPCSection(body, section, appendText string) (string, error) {
	header := "## " + section
	isLastUpdate := section == "Последнее обновление"
	cleaned := strings.TrimRight(appendText, "\n")
	if cleaned == "" {
		return body, nil
	}
	lines := strings.Split(body, "\n")
	headerIdx := -1
	for i, ln := range lines {
		if strings.TrimSpace(ln) == header {
			headerIdx = i
			break
		}
	}
	if headerIdx < 0 {
		// Section missing: insert it at the canonical
		// position. Walk canonicalNPCSections: the new
		// section lands right BEFORE the next section
		// that already exists in the file, or at end
		// of file if no later section is present.
		// We look for the FIRST canonical section that
		// comes AFTER the new one AND exists in the
		// file; that one becomes the anchor.
		insertAt := len(lines)
		newIdx := indexOfCanonical(section)
		for i := newIdx + 1; i < len(canonicalNPCSections); i++ {
			candidateHeader := "## " + canonicalNPCSections[i]
			for j, ln := range lines {
				if strings.TrimSpace(ln) == candidateHeader {
					insertAt = j
					break
				}
			}
			if insertAt != len(lines) {
				break
			}
		}
		var out []string
		out = append(out, lines[:insertAt]...)
		out = append(out, "", header, cleaned, "")
		out = append(out, lines[insertAt:]...)
		return strings.Join(out, "\n"), nil
	}
	// Section exists: find its end (next "## " header or EOF).
	endIdx := len(lines)
	for j := headerIdx + 1; j < len(lines); j++ {
		if strings.HasPrefix(strings.TrimSpace(lines[j]), "## ") {
			endIdx = j
			break
		}
	}
	if isLastUpdate {
		// REPLACE the section body — only the freshest
		// line lives here. If the model emits multi-line
		// text we keep all of it but trim surrounding
		// whitespace.
		newBody := []string{header, cleaned, ""}
		out := make([]string, 0, len(lines)+2)
		out = append(out, lines[:headerIdx]...)
		out = append(out, newBody...)
		if endIdx < len(lines) {
			out = append(out, lines[endIdx:]...)
		}
		return strings.Join(out, "\n"), nil
	}
	// Append: dedupe (in case the same fact is recorded
	// twice) and append the new line at the end of the
	// section.
	seen := make(map[string]struct{}, endIdx-headerIdx)
	for j := headerIdx + 1; j < endIdx; j++ {
		t := strings.TrimSpace(lines[j])
		if t != "" {
			seen[strings.ToLower(t)] = struct{}{}
		}
	}
	if _, dup := seen[strings.ToLower(cleaned)]; dup {
		return body, nil
	}
	insertAt := endIdx
	if insertAt > 0 && strings.TrimSpace(lines[insertAt-1]) == "" {
		// Body of section ends with a blank line; insert
		// the new line right before it to keep the
		// existing visual gap.
		insertAt--
	}
	out := make([]string, 0, len(lines)+1)
	out = append(out, lines[:insertAt]...)
	out = append(out, cleaned)
	if insertAt < len(lines) {
		out = append(out, lines[insertAt:]...)
	}
	return strings.Join(out, "\n"), nil
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

// Load returns the NPC's file contents. Returns
// ErrNPCNotFound when the file is missing or empty so
// callers (the GM's missedToolGuard, /me, and UpdateNPC)
// can distinguish "no profile yet" from "profile exists
// but is blank".
func (n *NPC) Load(world, npc string) (string, error) {
	name, err := domain.SanitizeName(npc)
	if err != nil {
		return "", fmt.Errorf("npc name: %w", err)
	}
	body, err := n.fs.ReadRaw("worlds/" + world + "/characters/" + name + ".md")
	if err != nil || body == "" {
		return "", ErrNPCNotFound
	}
	return body, nil
}

// CompactNPCBody strips dated events from an NPC file, leaving
// the persistent profile (temperament, relations, abilities,
// nicknames) intact. The function is pure — no I/O — so
// callers (Memory.CompactNPCs) control when to read / write.
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
