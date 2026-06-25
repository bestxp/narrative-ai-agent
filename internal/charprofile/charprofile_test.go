package charprofile

import (
	"errors"
	"strings"
	"testing"
)

// helper: build a Soul from a list of (section,
// values) pairs and assert it round-trips through
// yaml.Marshal.
func TestSoul_LoadSave_RoundTrip(t *testing.T) {
	t.Parallel()
	body := `name: Маркус Мрачный
soul: "13 лет"
data:
  - section: Истинная сущность
    values:
      - Ребёнок-попаданец
      - Выглядит на 11–12
  - section: Предпочтения
    values:
      - Любит животных
`
	s, err := LoadSoul(body)
	if err != nil {
		t.Fatalf("LoadSoul: %v", err)
	}
	if s.Name != "Маркус Мрачный" {
		t.Errorf("Name = %q", s.Name)
	}
	if s.Soul != "13 лет" {
		t.Errorf("Soul = %q", s.Soul)
	}
	if len(s.Data) != 2 {
		t.Fatalf("Data len = %d, want 2", len(s.Data))
	}
	if s.Data[0].Name != "Истинная сущность" || len(s.Data[0].Values) != 2 {
		t.Errorf("section 0 = %+v", s.Data[0])
	}
	out, err := s.Save()
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	// Re-parse the saved output to make sure it
	// is still valid YAML of the same shape.
	s2, err := LoadSoul(out)
	if err != nil {
		t.Fatalf("LoadSoul(saved): %v", err)
	}
	if s2.Name != s.Name || len(s2.Data) != len(s.Data) {
		t.Errorf("round-trip differs: %+v vs %+v", s, s2)
	}
}

func TestSoul_Load_Empty(t *testing.T) {
	t.Parallel()
	_, err := LoadSoul("")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestSoul_Append_NewSection(t *testing.T) {
	t.Parallel()
	var s Soul
	s.Name = "X"
	changed := s.Append("Предпочтения", "Любит кошек")
	if !changed {
		t.Fatal("expected change on first append")
	}
	changed = s.Append("Предпочтения", "Любит кошек")
	if changed {
		t.Fatal("expected idempotent on duplicate value")
	}
	if len(s.Data) != 1 || len(s.Data[0].Values) != 1 {
		t.Fatalf("Data = %+v", s.Data)
	}
}

func TestSoul_Append_AnySection(t *testing.T) {
	t.Parallel()
	var s Soul
	// Soul is free-form: an unknown section
	// is accepted, not dropped.
	if !s.Append("Свободная секция", "value") {
		t.Fatal("Soul.Append must accept any section name")
	}
}

func TestSkill_Append_StrictEnum(t *testing.T) {
	t.Parallel()
	var s Skill
	// Known section → accepted.
	if !s.Append("Оружие", "Кунай — 3 шт.") {
		t.Fatal("Оружие must be accepted")
	}
	// Unknown section → silently dropped (the
	// Skill contract is "section name must be
	// canonical, otherwise no-op"). The Tool
	// layer logs the slowlog event upstream.
	if s.Append("Misc", "garbage") {
		t.Fatal("Skill.Append must reject unknown sections")
	}
	if len(s.Data) != 1 {
		t.Fatalf("Data = %+v", s.Data)
	}
}

func TestSkill_Append_AllFixedSections(t *testing.T) {
	t.Parallel()
	var s Skill
	for _, sec := range SkillFixedSections {
		if !s.Append(sec, "v") {
			t.Errorf("section %q must be accepted", sec)
		}
	}
	if len(s.Data) != len(SkillFixedSections) {
		t.Errorf("expected %d sections, got %d", len(SkillFixedSections), len(s.Data))
	}
}

func TestMemory_Append_StrictEnum(t *testing.T) {
	t.Parallel()
	var m Memory
	for _, sec := range MemoryFixedSections {
		if !m.Append(sec, "v") {
			t.Errorf("section %q must be accepted", sec)
		}
	}
	if m.Append("Misc", "v") {
		t.Error("Memory.Append must reject unknown sections")
	}
}

func TestBase_ReplaceSection(t *testing.T) {
	t.Parallel()
	var s Soul
	s.Append("X", "old1")
	s.Append("X", "old2")
	if !s.ReplaceSection("X", "new") {
		t.Fatal("ReplaceSection must report change")
	}
	if s.Data[0].Values[0] != "new" || len(s.Data[0].Values) != 1 {
		t.Fatalf("values = %+v", s.Data[0].Values)
	}
	// Same value again → no-op.
	if s.ReplaceSection("X", "new") {
		t.Fatal("ReplaceSection must be idempotent on identical value")
	}
}

func TestBase_SortedSectionNames(t *testing.T) {
	t.Parallel()
	var s Skill
	s.Append("Ограничения", "x")
	s.Append("Оружие", "y")
	s.Append("Ранг", "z")
	got := s.SortedSectionNames()
	want := []string{"Ограничения", "Оружие", "Ранг"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("got %v, want %v", got, want)
	}
}

// --- Inventory ---

func TestInventory_LoadSave_RoundTrip(t *testing.T) {
	t.Parallel()
	body := `currency:
  - name: Рё
    count: 5000
  - name: Кредиты империи
    count: 1000000
items:
  - name: Кунай
    description: Стандартный клинок.
    equip: false
    special: нет
  - name: Клинок бога грома
    description: Чёрный клинок.
    equip: true
    special: привязан к душе
`
	inv, err := LoadInventory(body)
	if err != nil {
		t.Fatalf("LoadInventory: %v", err)
	}
	// inventory.yaml does not carry a `name` field —
	// the character identity is in SOUL.yaml.
	if len(inv.Currency) != 2 {
		t.Errorf("Currency len = %d", len(inv.Currency))
	}
	if inv.Currency[0].Name != "Рё" || inv.Currency[0].Count != 5000 {
		t.Errorf("Currency[0] = %+v", inv.Currency[0])
	}
	if len(inv.Items) != 2 {
		t.Errorf("Items len = %d", len(inv.Items))
	}
	if !inv.Items[1].Equip {
		t.Errorf("Items[1] not equipped: %+v", inv.Items[1])
	}
	out, err := inv.Save()
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	inv2, err := LoadInventory(out)
	if err != nil {
		t.Fatalf("LoadInventory(saved): %v", err)
	}
	if len(inv2.Items) != 2 {
		t.Errorf("round-trip items differ: %+v", inv2.Items)
	}
}

func TestInventory_Load_Empty(t *testing.T) {
	t.Parallel()
	_, err := LoadInventory("")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestInventory_AppendItem_New(t *testing.T) {
	t.Parallel()
	var inv Inventory
	if !inv.AppendItem(Item{Name: "Кунай", Equip: false, Special: "нет"}) {
		t.Fatal("expected change on first item")
	}
	if !inv.AppendItem(Item{Name: "Кунай", Equip: true, Special: "нет"}) {
		t.Fatal("expected change on REPLACE of existing item")
	}
	if len(inv.Items) != 1 {
		t.Fatalf("items len = %d", len(inv.Items))
	}
	if !inv.Items[0].Equip {
		t.Fatal("equip flag was not updated on REPLACE")
	}
}

func TestInventory_AppendItem_NoOpOnIdentical(t *testing.T) {
	t.Parallel()
	var inv Inventory
	inv.AppendItem(Item{Name: "X", Description: "d", Equip: true, Special: "s"})
	if inv.AppendItem(Item{Name: "X", Description: "d", Equip: true, Special: "s"}) {
		t.Fatal("AppendItem must report no-op on identical payload")
	}
}

func TestInventory_RemoveItem(t *testing.T) {
	t.Parallel()
	var inv Inventory
	inv.AppendItem(Item{Name: "A"})
	inv.AppendItem(Item{Name: "B"})
	if err := inv.RemoveItem("A"); err != nil {
		t.Fatalf("RemoveItem A: %v", err)
	}
	if len(inv.Items) != 1 || inv.Items[0].Name != "B" {
		t.Fatalf("items = %+v", inv.Items)
	}
	if err := inv.RemoveItem("A"); !errors.Is(err, ErrItemNotFound) {
		t.Fatalf("expected ErrItemNotFound, got %v", err)
	}
}

func TestInventory_RemoveItem_EmptyName(t *testing.T) {
	t.Parallel()
	var inv Inventory
	if err := inv.RemoveItem(""); !errors.Is(err, ErrItemNotFound) {
		t.Fatalf("expected ErrItemNotFound, got %v", err)
	}
}

func TestInventory_SetCurrency(t *testing.T) {
	t.Parallel()
	var inv Inventory
	if !inv.SetCurrency("Рё", 5000) {
		t.Fatal("expected change on new currency")
	}
	if inv.SetCurrency("Рё", 5000) {
		t.Fatal("expected no-op on identical count")
	}
	if !inv.SetCurrency("Рё", 4200) {
		t.Fatal("expected change on updated count")
	}
	if inv.Currency[0].Count != 4200 {
		t.Fatalf("count = %d", inv.Currency[0].Count)
	}
}

func TestInventory_SetCurrency_Clamp(t *testing.T) {
	t.Parallel()
	var inv Inventory
	inv.SetCurrency("Рё", -1)
	if inv.Currency[0].Count != 0 {
		t.Errorf("negative should clamp to 0, got %d", inv.Currency[0].Count)
	}
	inv.SetCurrency("Рё", 999_999_999+1)
	if inv.Currency[0].Count != 999_999_999 {
		t.Errorf("over-max should clamp, got %d", inv.Currency[0].Count)
	}
}

func TestInventory_RemoveCurrency(t *testing.T) {
	t.Parallel()
	var inv Inventory
	inv.SetCurrency("Рё", 100)
	if err := inv.RemoveCurrency("Рё"); err != nil {
		t.Fatalf("RemoveCurrency: %v", err)
	}
	if len(inv.Currency) != 0 {
		t.Fatalf("currency = %+v", inv.Currency)
	}
	if err := inv.RemoveCurrency("Рё"); !errors.Is(err, ErrItemNotFound) {
		t.Fatalf("expected ErrItemNotFound, got %v", err)
	}
}

// --- Migration ---

func TestMigrateFromMarkdown_Soul(t *testing.T) {
	t.Parallel()
	body := "# Маркус\n\n## Истинная сущность\n- Ребёнок\n- Сирота\n\n## Предпочтения\n- Любит кошек\n"
	got, err := MigrateFromMarkdown("SOUL", body, "markus")
	if err != nil {
		t.Fatalf("MigrateFromMarkdown: %v", err)
	}
	s, ok := got.(Soul)
	if !ok {
		t.Fatalf("expected Soul, got %T", got)
	}
	if s.Name != "Маркус" {
		t.Errorf("Name = %q", s.Name)
	}
	if len(s.Data) != 2 {
		t.Fatalf("Data = %+v", s.Data)
	}
	if s.Data[0].Name != "Истинная сущность" || len(s.Data[0].Values) != 2 {
		t.Errorf("section 0 = %+v", s.Data[0])
	}
}

func TestMigrateFromMarkdown_Skill(t *testing.T) {
	t.Parallel()
	body := "# M\n\n## Оружие\n- Кунай\n- Сюрикен\n\n## Ранг\n- Генин\n"
	got, err := MigrateFromMarkdown("skill", body, "m")
	s, ok := got.(Skill)
	if !ok {
		t.Fatalf("MigrateFromMarkdown: unexpected type %T", got)
	}
	if err != nil || len(s.Data) != 2 {
		t.Fatalf("MigrateFromMarkdown: %v / %+v", err, s)
	}
}

func TestMigrateFromMarkdown_Memory_NumberedList(t *testing.T) {
	t.Parallel()
	body := "# M\n\n## Яркие моменты\n1. Видение с Кагуей\n2. Первый поцелуй с Ино\n"
	got, err := MigrateFromMarkdown("memory", body, "m")
	m, ok := got.(Memory)
	if !ok {
		t.Fatalf("MigrateFromMarkdown: unexpected type %T", got)
	}
	if err != nil {
		t.Fatalf("MigrateFromMarkdown: %v", err)
	}
	if len(m.Data) != 1 || len(m.Data[0].Values) != 2 {
		t.Fatalf("Data = %+v", m.Data)
	}
	if m.Data[0].Values[0] != "Видение с Кагуей" {
		t.Errorf("values[0] = %q", m.Data[0].Values[0])
	}
}

func TestMigrateFromMarkdown_Empty(t *testing.T) {
	t.Parallel()
	_, err := MigrateFromMarkdown("SOUL", "", "x")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestMigrateFromMarkdown_UnknownKind(t *testing.T) {
	t.Parallel()
	_, err := MigrateFromMarkdown("garbage", "body", "x")
	if !errors.Is(err, ErrUnknownFile) {
		t.Fatalf("want ErrUnknownFile, got %v", err)
	}
}

func TestMigrateFromMarkdown_Skill_DropsUnknownSection(t *testing.T) {
	t.Parallel()
	body := "# M\n\n## Ранг\n- Генин\n\n## MiscGarbage\n- drop me\n"
	got, _ := MigrateFromMarkdown("skill", body, "m")
	s, ok := got.(Skill)
	if !ok {
		t.Fatalf("migrate skill: unexpected return type %T", got)
	}
	if len(s.Data) != 1 {
		t.Fatalf("MiscGarbage must be dropped on strict skill, got %+v", s.Data)
	}
}

func TestMigrateFromMarkdown_Soul_KeepsUnknownSection(t *testing.T) {
	t.Parallel()
	// Soul is free-form — non-canonical section
	// names are kept verbatim. (Legacy free-form
	// files used headings like "Внешность",
	// "Предыстория", "Глаза" — we accept all of
	// them.)
	body := "# M\n\n## Свободная секция\n- v\n"
	got, _ := MigrateFromMarkdown("SOUL", body, "m")
	s, ok := got.(Soul)
	if !ok {
		t.Fatalf("migrate SOUL: unexpected return type %T", got)
	}
	if len(s.Data) != 1 || s.Data[0].Name != "Свободная секция" {
		t.Fatalf("Soul must keep free-form section, got %+v", s.Data)
	}
}

// TestMigrateFromMarkdown_Memory_KeepsAllSections is the
// regression test for the data-loss bug: legacy
// memory.md had ~20 free-form ## sections
// (Видения, Контакты семьи Яманака, etc.) that
// the strict migration dropped silently, leaving
// the new memory.yaml empty.
//
// MigrateFromMarkdown is LOSS-LESS — see the
// data-preservation contract. The strict enum is
// enforced only at Append / ReplaceSection.
func TestMigrateFromMarkdown_Memory_KeepsAllSections(t *testing.T) {
	t.Parallel()
	body := "# M\n\n## Видения Кагуи\n- сон 1\n- сон 2\n\n## Контакт с семьёй Яманака\n- тётя\n\n## Яркие моменты\n- первый поцелуй\n\n## Действия дня 1\n- душ\n- завтрак\n"
	got, err := MigrateFromMarkdown("memory", body, "m")
	if err != nil {
		t.Fatalf("MigrateFromMarkdown: %v", err)
	}
	m, ok := got.(Memory)
	if !ok {
		t.Fatalf("migrate memory: unexpected return type %T", got)
	}
	if len(m.Data) != 4 {
		t.Fatalf("memory migration must keep all 4 legacy sections, got %+v", m.Data)
	}
	want := map[string]int{
		"Видения Кагуи":            2,
		"Контакт с семьёй Яманака": 1,
		"Яркие моменты":            1,
		"Действия дня 1":           2,
	}
	for _, sec := range m.Data {
		if w, ok := want[sec.Name]; !ok {
			t.Errorf("unexpected section %q", sec.Name)
		} else if len(sec.Values) != w {
			t.Errorf("%q: want %d values, got %d", sec.Name, w, len(sec.Values))
		}
	}
}
