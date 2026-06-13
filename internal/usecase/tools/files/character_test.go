package files

import (
	"errors"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/bestxp/narrative-ai-agent/internal/adapter/storage"
	"github.com/bestxp/narrative-ai-agent/internal/charprofile"
	"github.com/bestxp/narrative-ai-agent/internal/slowlog"
	"github.com/bestxp/narrative-ai-agent/internal/usecase/tools"
)

// newTestCharacter builds a Character with an
// ephemeral FileStore rooted in t.TempDir(). The
// caller is responsible for any further dir/file
// seeding.
func newTestCharacter(t *testing.T) (*Character, *storage.FileStore) {
	t.Helper()
	fs, _ := storage.NewFileStore(t.TempDir())
	c := newCharacter(fs, zerolog.Nop(), slowlog.Discard())
	return c, fs
}

// --- Append routing ---

func TestCharacter_Append_DispatchesByKind(t *testing.T) {
	c, _ := newTestCharacter(t)
	if err := c.fs.EnsureDir("characters/markus"); err != nil {
		t.Fatal(err)
	}
	// Soul.
	if err := c.Append("markus", "SOUL", "Предпочтения", "любит кошек"); err != nil {
		t.Fatalf("Append SOUL: %v", err)
	}
	// Skill — section must be on the enum.
	if err := c.Append("markus", "skill", "Оружие", "Кунай — 3 шт."); err != nil {
		t.Fatalf("Append skill: %v", err)
	}
	// Memory — section must be on the enum.
	if err := c.Append("markus", "memory", "Яркие моменты", "первый поцелуй с Ино"); err != nil {
		t.Fatalf("Append memory: %v", err)
	}
	// Unknown file kind.
	if err := c.Append("markus", "garbage", "x", "y"); !errors.Is(err, ErrUnknownCharacterFile) {
		t.Fatalf("want ErrUnknownCharacterFile, got %v", err)
	}
}

func TestCharacter_AppendSkill_RejectsUnknownSection(t *testing.T) {
	c, _ := newTestCharacter(t)
	if err := c.Append("markus", "skill", "Оружие", "v"); err != nil {
		t.Fatal(err)
	}
	// "Misc" is not on charprofile.SkillFixedSections.
	if err := c.Append("markus", "skill", "Misc", "v"); !errors.Is(err, charprofile.ErrSectionNotFound) {
		t.Fatalf("want ErrSectionNotFound, got %v", err)
	}
}

func TestCharacter_AppendMemorySection_RejectsUnknownSection(t *testing.T) {
	c, _ := newTestCharacter(t)
	if err := c.Append("markus", "memory", "Яркие моменты", "v"); err != nil {
		t.Fatal(err)
	}
	if err := c.Append("markus", "memory", "Бред", "v"); !errors.Is(err, charprofile.ErrSectionNotFound) {
		t.Fatalf("want ErrSectionNotFound, got %v", err)
	}
}

func TestCharacter_Append_EmptyArgs(t *testing.T) {
	c, _ := newTestCharacter(t)
	if err := c.Append("markus", "SOUL", "", "v"); !errors.Is(err, ErrEmptySection) {
		t.Fatalf("empty section, got %v", err)
	}
	if err := c.Append("markus", "SOUL", "X", ""); !errors.Is(err, ErrEmptyAppend) {
		t.Fatalf("empty append, got %v", err)
	}
	if err := c.Append("", "SOUL", "X", "v"); !errors.Is(err, ErrNoActiveCharacter) {
		t.Fatalf("empty dir, got %v", err)
	}
}

// --- Inventory ---

func TestCharacter_AppendInventoryItem_AddAndReplace(t *testing.T) {
	c, _ := newTestCharacter(t)
	if err := c.fs.EnsureDir("characters/markus"); err != nil {
		t.Fatal(err)
	}
	changed, err := c.AppendInventoryItem("markus", charprofile.Item{
		Name: "Кунай", Description: "Стандартный клинок", Equip: false, Special: "нет",
	})
	if err != nil || !changed {
		t.Fatalf("first append: changed=%v err=%v", changed, err)
	}
	// Re-append same name with different equip: REPLACE.
	changed, err = c.AppendInventoryItem("markus", charprofile.Item{
		Name: "Кунай", Description: "Стандартный клинок", Equip: true, Special: "нет",
	})
	if err != nil || !changed {
		t.Fatalf("replace: changed=%v err=%v", changed, err)
	}
	// Identical payload: no-op.
	changed, _ = c.AppendInventoryItem("markus", charprofile.Item{
		Name: "Кунай", Description: "Стандартный клинок", Equip: true, Special: "нет",
	})
	if changed {
		t.Fatal("identical payload should be no-op")
	}
}

func TestCharacter_RemoveInventoryItem(t *testing.T) {
	c, _ := newTestCharacter(t)
	c.fs.EnsureDir("characters/markus")
	c.AppendInventoryItem("markus", charprofile.Item{Name: "A"})
	c.AppendInventoryItem("markus", charprofile.Item{Name: "B"})
	if err := c.RemoveInventoryItem("markus", "A"); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveInventoryItem("markus", "missing"); !errors.Is(err, charprofile.ErrItemNotFound) {
		t.Fatalf("want ErrItemNotFound, got %v", err)
	}
}

func TestCharacter_SetCurrency(t *testing.T) {
	c, _ := newTestCharacter(t)
	c.fs.EnsureDir("characters/markus")
	if changed, _ := c.SetCurrency("markus", "Рё", 5000); !changed {
		t.Fatal("first set: expected change")
	}
	if changed, _ := c.SetCurrency("markus", "Рё", 5000); changed {
		t.Fatal("identical set: expected no-op")
	}
	if changed, _ := c.SetCurrency("markus", "Рё", 4200); !changed {
		t.Fatal("update: expected change")
	}
}

// --- Read / snapshot ---

func TestCharacter_Read_LoadsAllFourFiles(t *testing.T) {
	c, fs := newTestCharacter(t)
	if err := fs.EnsureDir("characters/markus"); err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteRawAtomic("characters/markus/SOUL.yaml", "name: M\ndata: []\n"); err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteRawAtomic("characters/markus/skill.yaml", "name: M\ndata: []\n"); err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteRawAtomic("characters/markus/memory.yaml", "name: M\ndata: []\n"); err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteRawAtomic("characters/markus/inventory.yaml", "name: M\nitems: []\n"); err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteRawAtomic("worlds/naruto/state.md", "День 3\nNPC: Какаши\n"); err != nil {
		t.Fatal(err)
	}
	snap, err := c.Read("markus", "naruto")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if snap.Character != "markus" || snap.World != "naruto" || snap.Day != 3 {
		t.Errorf("snap = %+v", snap)
	}
	if !strings.Contains(snap.SOUL, "name: M") {
		t.Errorf("SOUL not loaded: %q", snap.SOUL)
	}
	if !strings.Contains(snap.Inventory, "items: []") {
		t.Errorf("Inventory not loaded: %q", snap.Inventory)
	}
}

// --- Migration ---

func TestCharacter_MigrateLegacy_NoLegacyFiles(t *testing.T) {
	c, _ := newTestCharacter(t)
	c.fs.EnsureDir("characters/markus")
	conv, err := c.MigrateLegacy(nil, "markus", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(conv) != 0 {
		t.Errorf("expected no conversions, got %v", conv)
	}
}

func TestCharacter_MigrateLegacy_SoulOnly(t *testing.T) {
	c, fs := newTestCharacter(t)
	if err := fs.EnsureDir("characters/markus"); err != nil {
		t.Fatal(err)
	}
	// Legacy .md.
	if err := fs.WriteRawAtomic("characters/markus/SOUL.md",
		"# Маркус\n\n## Истинная сущность\n- Ребёнок\n- Сирота\n\n## Предпочтения\n- Любит кошек\n"); err != nil {
		t.Fatal(err)
	}
	conv, err := c.MigrateLegacy(nil, "markus", "naruto")
	if err != nil {
		t.Fatal(err)
	}
	if len(conv) != 1 || conv[0] != "SOUL" {
		t.Fatalf("conv = %v", conv)
	}
	// YAML written.
	body, _ := fs.ReadRaw("characters/markus/SOUL.yaml")
	if !strings.Contains(body, "Ребёнок") {
		t.Errorf("YAML missing value: %q", body)
	}
	// Legacy renamed to .bak.
	body, _ = fs.ReadRaw("characters/markus/SOUL.md.bak")
	if !strings.Contains(body, "Истинная сущность") {
		t.Errorf("bak missing: %q", body)
	}
}

func TestCharacter_MigrateLegacy_AllThree(t *testing.T) {
	c, fs := newTestCharacter(t)
	if err := fs.EnsureDir("characters/markus"); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"SOUL", "SKILL", "memory"} {
		_ = fs.WriteRawAtomic("characters/markus/"+k+".md",
			"# M\n\n## Истинная сущность\n- v\n")
	}
	conv, _ := c.MigrateLegacy(nil, "markus", "naruto")
	if len(conv) != 3 {
		t.Errorf("expected 3 conversions, got %v", conv)
	}
}

func TestCharacter_MigrateLegacy_SkipsWhenYAMLPreexists(t *testing.T) {
	c, fs := newTestCharacter(t)
	if err := fs.EnsureDir("characters/markus"); err != nil {
		t.Fatal(err)
	}
	_ = fs.WriteRawAtomic("characters/markus/SOUL.md", "old legacy\n")
	_ = fs.WriteRawAtomic("characters/markus/SOUL.yaml", "name: M\ndata: []\n")
	conv, _ := c.MigrateLegacy(nil, "markus", "")
	if len(conv) != 0 {
		t.Errorf("expected no-op when YAML preexists, got %v", conv)
	}
}

// --- FormatSnapshot ---

func TestFormatSnapshot_RendersAllSections(t *testing.T) {
	snap := &tools.CharacterSnapshot{
		Character: "M",
		World:     "W",
		SOUL:      "soul body",
		SKILL:     "skill body",
		Memory:    "memory body",
		Inventory: "inv body",
		State:     "state body",
		Day:       5,
	}
	out := FormatSnapshot(snap, 40)
	for _, want := range []string{"SOUL.yaml", "skill.yaml", "memory.yaml", "inventory.yaml", "state.md", "день 5"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}
