// migrate-char is a one-shot CLI tool that converts a
// legacy free-form `characters/<dir>/{SOUL,SKILL,memory}.md`
// trio into the new YAML schema:
//
//	SOUL.yaml     — who the GG is (free-form sections)
//	skill.yaml    — what the GG can do (fixed enum sections)
//	memory.yaml   — what the GG remembers (fixed enum sections)
//	inventory.yaml — what the GG has (NEW, empty by default)
//
// The deterministic parser in charprofile.MigrateFromMarkdown
// is the only path used here — the LLM-driven path lives
// in tools/files/character.go (Migrator interface) and runs
// at first launch when the legacy .md is detected. The CLI
// is for the operator who already has a populated game-data
// tree and wants to migrate it once without losing state.
//
// Usage:
//
//	migrate-char --root /path/to/game-data --character markus --dry-run
//	migrate-char --root ./game-data --character markus
//
// Flags:
//
//	--root       path to game-data root (default "game-data")
//	--character  character dir name to migrate (required)
//	--dry-run    print what would change without writing
//	--state-md   path under <root>/worlds/<active>/ (default
//	             reads from info.yaml; permanent party moves
//	             from SKILL.md to state.md if found)
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/bestxp/narrative-ai-agent/internal/charprofile"
	"gopkg.in/yaml.v3"
)

func main() {
	root := flag.String("root", "game-data", "path to game-data root")
	charDir := flag.String("character", "", "character dir name (required)")
	dryRun := flag.Bool("dry-run", false, "print what would change without writing")
	flag.Parse()

	if *charDir == "" {
		log.Fatalf("--character is required (the dir name under characters/)")
	}

	abs, err := filepath.Abs(*root)
	if err != nil {
		log.Fatalf("abs: %v", err)
	}

	charRoot := filepath.Join(abs, "characters", *charDir)
	if _, err := os.Stat(charRoot); err != nil {
		log.Fatalf("character dir %q: %v", charRoot, err)
	}

	// Detect info.yaml → world (for permanent-party move).
	world := readActiveWorld(filepath.Join(abs, "info.yaml"))

	// 1) SOUL.md → SOUL.yaml
	// 2) SKILL.md → skill.yaml (extract permanent party separately)
	// 3) memory.md → memory.yaml
	// 4) Create empty inventory.yaml
	// 5) Move ## permanent party from SKILL.md to state.md
	soulBody := readFileOr(filepath.Join(charRoot, "SOUL.md"), "")
	skillBody := readFileOr(filepath.Join(charRoot, "SKILL.md"), "")
	memBody := readFileOr(filepath.Join(charRoot, "memory.md"), "")

	var converted []string
	var skipped []string
	yamlByKind := map[string]string{}

	if soulBody != "" {
		raw, err := charprofile.MigrateFromMarkdown("SOUL", soulBody, *charDir)
		if err != nil {
			log.Fatalf("migrate SOUL: %v", err)
		}
		s, ok := raw.(charprofile.Soul)
		if !ok {
			log.Fatalf("SOUL migration returned %T, want Soul", raw)
		}
		// Drop legacy "## Действия дня N" sections from SOUL.
		// Those were a free-form journal that lived in
		// the same file as the character core in the old
		// schema; in the new schema they belong to
		// memorise.md (per-day facts) and state.md
		// (current scene). They will NOT be lost — the
		// originals stay in SOUL.md.bak after migration.
		filtered := s.Data[:0]
		for _, sec := range s.Data {
			if strings.HasPrefix(sec.Name, "Действия дня") {
				continue
			}
			filtered = append(filtered, sec)
		}
		s.Data = filtered
		// MigrateFromMarkdown set s.Name from the H1
		// line. The legacy convention was "# <Name> —
		// <subtitle>" (e.g. "# Маркус — Ядро персонажа").
		// The charprofile layer only knows the bare
		// name, so we strip the suffix here. We use
		// em-dash as the split separator (the Russian
		// convention in this codebase).
		if idx := strings.Index(s.Name, " — "); idx > 0 {
			s.Name = strings.TrimSpace(s.Name[:idx])
		} else if idx := strings.Index(s.Name, " - "); idx > 0 {
			s.Name = strings.TrimSpace(s.Name[:idx])
		}
		// Try to populate s.Soul from the first
		// values[] of the "Истинная сущность" section
		// (the most stable "who is this character"
		// one-liner in the legacy SOUL.md). Fall back
		// to the first values[] of any section if that
		// specific section is missing. The LLM-driven
		// path (Migrator interface in character.go) is
		// the right tool to refine this later.
		if s.Soul == "" {
			for _, sec := range s.Data {
				if sec.Name == "Истинная сущность" && len(sec.Values) > 0 {
					s.Soul = sec.Values[0]
					break
				}
			}
		}
		if s.Soul == "" {
			for _, sec := range s.Data {
				if len(sec.Values) > 0 {
					s.Soul = sec.Values[0]
					break
				}
			}
		}
		body, err := s.Save()
		if err != nil {
			log.Fatalf("save SOUL: %v", err)
		}
		yamlByKind["SOUL"] = body
		converted = append(converted, "SOUL")
	} else {
		skipped = append(skipped, "SOUL.md (missing)")
	}

	if skillBody != "" {
		raw, err := charprofile.MigrateFromMarkdown("skill", skillBody, *charDir)
		if err != nil {
			log.Fatalf("migrate skill: %v", err)
		}
		s, ok := raw.(charprofile.Skill)
		if !ok {
			log.Fatalf("skill migration returned %T, want Skill", raw)
		}
		body, err := s.Save()
		if err != nil {
			log.Fatalf("save skill: %v", err)
		}
		yamlByKind["skill"] = body
		converted = append(converted, "skill")
	} else {
		skipped = append(skipped, "SKILL.md (missing)")
	}

	if memBody != "" {
		// Count legacy sections that are NOT on the
		// canonical 4-section enum. The strict enum
		// is enforced only at Append-time; the
		// migration path keeps every section. This
		// counter feeds the operator-facing INFO log
		// so they know how many free-form sections
		// the model will eventually need to refile.
		preserved := countMemorySectionsNotOnEnum(memBody)
		raw, err := charprofile.MigrateFromMarkdown("memory", memBody, *charDir)
		if err != nil {
			log.Fatalf("migrate memory: %v", err)
		}
		m, ok := raw.(charprofile.Memory)
		if !ok {
			log.Fatalf("memory migration returned %T, want Memory", raw)
		}
		// memory.yaml uses a STRICT 4-section enum AT
		// WRITE TIME (Append / ReplaceSection). The
		// MIGRATION path, however, is LOSS-LESS — every
		// `## <section>` from the legacy .md is kept
		// verbatim, even names that are not on the
		// canonical enum ("## Видения Кагуи",
		// "## Контакт с семьёй Яманака",
		// "## Действия дня 1", etc.). The model can
		// then refile values into a canonical bucket
		// on the next Append — but the data is
		// preserved on disk.
		//
		// Rationale: the previous strict migration
		// dropped 17 sections silently, leaving the
		// new memory.yaml empty and losing the
		// player's actual history. The strict enum
		// is a forward-looking contract for the
		// model, not a backwards-looking filter
		// for legacy data.
		if preserved > 0 {
			log.Printf("INFO: %d legacy memory sections preserved verbatim (kept on memory.yaml even though they are not on the 4-section enum).", preserved)
		}
		body, err := m.Save()
		if err != nil {
			log.Fatalf("save memory: %v", err)
		}
		yamlByKind["memory"] = body
		converted = append(converted, "memory")
	} else {
		skipped = append(skipped, "memory.md (missing)")
	}

	// Empty inventory.yaml (the legacy free-form had no
	// inventory at all — money + items lived as prose in
	// SKILL.md "Универсальные навыки" section, e.g. "Осталось
	// ~4210 рё из 5000"). The operator can repopulate by
	// hand from the narrative, or via the running bot's
	// set_currency tool calls.
	inv := charprofile.Inventory{}
	invBody, err := inv.Save()
	if err != nil {
		log.Fatalf("save inventory: %v", err)
	}
	yamlByKind["inventory"] = invBody
	converted = append(converted, "inventory")

	// Extract permanent party from SKILL.md (legacy
	// location) and inject into state.md.
	var ppNames []string
	if skillBody != "" {
		ppNames = extractPermanentParty(skillBody)
	}
	var stateMD string
	if world != "" {
		stateMD = filepath.Join(abs, "worlds", world, "state.md")
	} else {
		log.Print("WARN: no active world in info.yaml; permanent party will not be moved")
	}

	// Plan: print + apply
	fmt.Println("=== PLAN ===")
	for _, f := range converted {
		fmt.Printf("  + characters/%s/%s.yaml\n", *charDir, f)
	}
	for _, f := range skipped {
		fmt.Printf("  - skipped: characters/%s/%s (missing)\n", *charDir, f)
	}
	if len(ppNames) > 0 && world != "" {
		fmt.Printf("  + worlds/%s/state.md : append '## permanent party' = %v\n", world, ppNames)
	} else if len(ppNames) > 0 {
		fmt.Printf("  - permanent party found in SKILL.md but no active_world; SKIPPING\n")
	}
	if *dryRun {
		fmt.Println("\n=== DRY-RUN — nothing written ===")
		fmt.Println("YAML previews:")
		for k, v := range yamlByKind {
			fmt.Printf("\n--- %s.yaml ---\n%s\n", k, v)
		}
		return
	}

	// Write YAML files.
	for k, body := range yamlByKind {
		target := filepath.Join(charRoot, k+".yaml")
		if err := writeFileAtomic(target, body); err != nil {
			log.Fatalf("write %s: %v", target, err)
		}
		fmt.Printf("  wrote %s (%d bytes)\n", target, len(body))
	}

	// Move permanent party.
	if len(ppNames) > 0 && stateMD != "" {
		cur, _ := os.ReadFile(stateMD)
		if !strings.Contains(string(cur), "## permanent party") {
			marker := fmt.Sprintf("\n\n## permanent party\n%s\n", strings.Join(ppNames, ", "))
			next := string(cur)
			if !strings.HasSuffix(next, "\n") {
				next += "\n"
			}
			next += marker
			if err := writeFileAtomic(stateMD, next); err != nil {
				log.Fatalf("write state.md: %v", err)
			}
			fmt.Printf("  wrote %s (appended permanent party)\n", stateMD)
		} else {
			fmt.Printf("  - state.md already has '## permanent party'; not duplicating\n")
		}
	}

	// Rename legacy .md → .bak (NOT delete; operator
	// keeps the originals until they verify the YAML is
	// right). We rename only the source files that were
	// successfully converted.
	for _, name := range []string{"SOUL.md", "SKILL.md", "memory.md"} {
		p := filepath.Join(charRoot, name)
		if _, err := os.Stat(p); err == nil {
			if err := os.Rename(p, p+".bak"); err != nil {
				log.Printf("WARN: rename %s: %v", p, err)
			} else {
				fmt.Printf("  renamed %s -> %s.bak\n", p, p)
			}
		}
	}

	fmt.Println("\n=== DONE ===")
}

// readFileOr returns body if the file exists, or "" if
// it does not (a missing file is not a fatal error — the
// operator might be migrating a half-set character).
func readFileOr(path, fallback string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return fallback
	}
	return string(b)
}

// writeFileAtomic writes body to path via a temp
// file + rename, matching the FileStore semantics
// (no torn writes on crash).
func writeFileAtomic(path, body string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(body), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// extractPermanentParty parses a "## permanent party"
// section out of the legacy SKILL.md body and returns
// the comma-separated names from the first non-empty
// line under the header. The result is a flat slice of
// trimmed, non-empty names. Mirrors the format that
// extractPermanentParty in gm.go understands (so the
// state.md output round-trips correctly through the
// running bot).
func extractPermanentParty(skillBody string) []string {
	const marker = "## permanent party"
	idx := strings.Index(skillBody, marker)
	if idx < 0 {
		return nil
	}
	rest := skillBody[idx+len(marker):]
	// Up to the next "## " sibling header or end of body.
	end := strings.Index(rest, "\n## ")
	if end < 0 {
		end = len(rest)
	}
	body := rest[:end]
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Drop the markdown bullet if present.
		line = strings.TrimPrefix(line, "- ")
		var out []string
		for _, n := range strings.Split(line, ",") {
			if t := strings.TrimSpace(n); t != "" {
				out = append(out, t)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

// readActiveWorld parses the legacy info.yaml and
// returns the active_world field. Returns "" if the
// file is missing or unparseable. We do not pull in
// the domain package here because it would re-introduce
// the cycle (charprofile already imports domain).
func readActiveWorld(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	// Lightweight parse: only need active_world.
	var raw struct {
		ActiveWorld string `yaml:"active_world"`
	}
	if err := yaml.Unmarshal(b, &raw); err != nil {
		return ""
	}
	return raw.ActiveWorld
}

// countMemorySectionsNotOnEnum walks the raw memory
// body and counts "## <name>" sections whose name is
// not in the strict 4-section enum. Used to surface
// a concrete operator-facing INFO log before the
// migration runs. The migration itself is
// loss-less — see MigrateFromMarkdown — so the
// count is "free-form sections preserved on
// memory.yaml" rather than "sections dropped".
// Returns 0 for empty bodies or files with no ##
// sections.
func countMemorySectionsNotOnEnum(body string) int {
	kept := map[string]bool{}
	for _, s := range charprofile.MemoryFixedSections {
		kept[s] = true
	}
	count := 0
	for _, line := range strings.Split(body, "\n") {
		t := strings.TrimSpace(line)
		if !strings.HasPrefix(t, "## ") {
			continue
		}
		name := strings.TrimSpace(t[3:])
		if name == "" {
			continue
		}
		if !kept[name] {
			count++
		}
	}
	return count
}
