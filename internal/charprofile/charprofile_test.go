package charprofile_test

import (
	"strings"
	"testing"

	"github.com/bestxp/narrative-ai-agent/internal/charprofile"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// helper: build a charprofile.Soul from a list of (section,
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

	s, err := charprofile.LoadSoul(body)
	require.NoError(t, err, "charprofile.LoadSoul")

	assert.Equal(t, "Маркус Мрачный", s.Name)
	assert.Equal(t, "13 лет", s.Soul)
	require.Len(t, s.Data, 2)
	assert.Equal(t, "Истинная сущность", s.Data[0].Name)
	assert.Len(t, s.Data[0].Values, 2)

	out, err := s.Save()
	require.NoError(t, err, "Save")
	// Re-parse the saved output to make sure it
	// is still valid YAML of the same shape.
	s2, err := charprofile.LoadSoul(out)
	require.NoError(t, err, "charprofile.LoadSoul(saved)")

	assert.Equal(t, s.Name, s2.Name)
	assert.Len(t, s2.Data, len(s.Data))
}

func TestSoul_Load_Empty(t *testing.T) {
	t.Parallel()

	_, err := charprofile.LoadSoul("")
	require.ErrorIs(t, err, charprofile.ErrNotFound, "want charprofile.ErrNotFound")
}

func TestSoul_Append_NewSection(t *testing.T) {
	t.Parallel()

	var s charprofile.Soul

	s.Name = "X"

	changed := s.Append("Предпочтения", "Любит кошек")
	require.True(t, changed, "expected change on first append")

	changed = s.Append("Предпочтения", "Любит кошек")
	require.False(t, changed, "expected idempotent on duplicate value")

	require.Len(t, s.Data, 1)
	require.Len(t, s.Data[0].Values, 1)
}

func TestSoul_Append_AnySection(t *testing.T) {
	t.Parallel()

	var s charprofile.Soul
	// charprofile.Soul is free-form: an unknown section
	// is accepted, not dropped.
	require.True(t, s.Append("Свободная секция", "value"), "charprofile.Soul.Append must accept any section name")
}

func TestSkill_Append_StrictEnum(t *testing.T) {
	t.Parallel()

	var s charprofile.Skill
	// Known section → accepted.
	require.True(t, s.Append("Оружие", "Кунай — 3 шт."), "Оружие must be accepted")
	// Unknown section → silently dropped (the
	// charprofile.Skill contract is "section name must be
	// canonical, otherwise no-op"). The Tool
	// layer logs the slowlog event upstream.
	require.False(t, s.Append("Misc", "garbage"), "charprofile.Skill.Append must reject unknown sections")

	require.Len(t, s.Data, 1)
}

func TestSkill_Append_AllFixedSections(t *testing.T) {
	t.Parallel()

	var s charprofile.Skill
	for _, sec := range charprofile.SkillFixedSections {
		assert.True(t, s.Append(sec, "v"), "section %q must be accepted", sec)
	}

	assert.Len(t, s.Data, len(charprofile.SkillFixedSections))
}

func TestMemory_Append_StrictEnum(t *testing.T) {
	t.Parallel()

	var m charprofile.Memory
	for _, sec := range charprofile.MemoryFixedSections {
		assert.True(t, m.Append(sec, "v"), "section %q must be accepted", sec)
	}

	assert.False(t, m.Append("Misc", "v"), "charprofile.Memory.Append must reject unknown sections")
}

func TestBase_ReplaceSection(t *testing.T) {
	t.Parallel()

	var s charprofile.Soul
	s.Append("X", "old1")
	s.Append("X", "old2")

	require.True(t, s.ReplaceSection("X", "new"), "ReplaceSection must report change")

	require.Equal(t, "new", s.Data[0].Values[0])
	require.Len(t, s.Data[0].Values, 1)
	// Same value again → no-op.
	require.False(t, s.ReplaceSection("X", "new"), "ReplaceSection must be idempotent on identical value")
}

func TestBase_SortedSectionNames(t *testing.T) {
	t.Parallel()

	var s charprofile.Skill
	s.Append("Ограничения", "x")
	s.Append("Оружие", "y")
	s.Append("Ранг", "z")
	got := s.SortedSectionNames()

	want := []string{"Ограничения", "Оружие", "Ранг"}
	assert.Equal(t, strings.Join(want, ","), strings.Join(got, ","))
}

// --- charprofile.Inventory ---

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

	inv, err := charprofile.LoadInventory(body)
	require.NoError(t, err, "charprofile.LoadInventory")
	// inventory.yaml does not carry a `name` field —
	// the character identity is in SOUL.yaml.
	assert.Len(t, inv.Currency, 2)
	assert.Equal(t, "Рё", inv.Currency[0].Name)
	assert.Equal(t, 5000, inv.Currency[0].Count)
	assert.Len(t, inv.Items, 2)
	assert.True(t, inv.Items[1].Equip, "Items[1] must be equipped")

	out, err := inv.Save()
	require.NoError(t, err, "Save")

	inv2, err := charprofile.LoadInventory(out)
	require.NoError(t, err, "charprofile.LoadInventory(saved)")

	assert.Len(t, inv2.Items, 2)
}

func TestInventory_Load_Empty(t *testing.T) {
	t.Parallel()

	_, err := charprofile.LoadInventory("")
	require.ErrorIs(t, err, charprofile.ErrNotFound)
}

func TestInventory_AppendItem_New(t *testing.T) {
	t.Parallel()

	var inv charprofile.Inventory

	require.True(t,
		inv.AppendItem(charprofile.Item{Name: "Кунай", Equip: false, Special: "нет"}),
		"expected change on first item")
	require.True(t,
		inv.AppendItem(charprofile.Item{Name: "Кунай", Equip: true, Special: "нет"}),
		"expected change on REPLACE of existing item")

	require.Len(t, inv.Items, 1)
	require.True(t, inv.Items[0].Equip, "equip flag was not updated on REPLACE")
}

func TestInventory_AppendItem_NoOpOnIdentical(t *testing.T) {
	t.Parallel()

	var inv charprofile.Inventory
	inv.AppendItem(charprofile.Item{Name: "X", Description: "d", Equip: true, Special: "s"})

	require.False(t,
		inv.AppendItem(charprofile.Item{Name: "X", Description: "d", Equip: true, Special: "s"}),
		"AppendItem must report no-op on identical payload")
}

func TestInventory_RemoveItem(t *testing.T) {
	t.Parallel()

	var inv charprofile.Inventory
	inv.AppendItem(charprofile.Item{Name: "A"})
	inv.AppendItem(charprofile.Item{Name: "B"})

	require.NoError(t, inv.RemoveItem("A"), "RemoveItem A")
	require.Len(t, inv.Items, 1)
	require.Equal(t, "B", inv.Items[0].Name)

	require.ErrorIs(t, inv.RemoveItem("A"), charprofile.ErrItemNotFound)
}

func TestInventory_RemoveItem_EmptyName(t *testing.T) {
	t.Parallel()

	var inv charprofile.Inventory
	require.ErrorIs(t, inv.RemoveItem(""), charprofile.ErrItemNotFound)
}

func TestInventory_SetCurrency(t *testing.T) {
	t.Parallel()

	var inv charprofile.Inventory
	require.True(t, inv.SetCurrency("Рё", 5000), "expected change on new currency")
	require.False(t, inv.SetCurrency("Рё", 5000), "expected no-op on identical count")
	require.True(t, inv.SetCurrency("Рё", 4200), "expected change on updated count")
	require.Equal(t, 4200, inv.Currency[0].Count)
}

func TestInventory_SetCurrency_Clamp(t *testing.T) {
	t.Parallel()

	var inv charprofile.Inventory
	inv.SetCurrency("Рё", -1)
	assert.Equal(t, 0, inv.Currency[0].Count, "negative should clamp to 0")

	inv.SetCurrency("Рё", 999_999_999+1)
	assert.Equal(t, 999_999_999, inv.Currency[0].Count, "over-max should clamp")
}

func TestInventory_RemoveCurrency(t *testing.T) {
	t.Parallel()

	var inv charprofile.Inventory
	inv.SetCurrency("Рё", 100)

	require.NoError(t, inv.RemoveCurrency("Рё"), "RemoveCurrency")
	require.Empty(t, inv.Currency)

	require.ErrorIs(t, inv.RemoveCurrency("Рё"), charprofile.ErrItemNotFound)
}
