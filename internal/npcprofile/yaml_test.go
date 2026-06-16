package npcprofile

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const fullYAML = `display_name: "Хината Хьюга"
file_slug: "hinata_hyuga"
temperament: "Застенчивая, но решительная в бою"
relations_gg: "Восхищается Маркусом, краснеет"
relations_npcs:
  - target: "Наруто"
    note: "Сильные чувства, скрывает"
  - target: "Маркус"
    note: "Доверяет"
abilities:
  - "Бьякуган — bloodline limit"
  - "Базовые техники Хьюга"
personal_memory:
  - "День 1: познакомилась с Маркусом на поляне"
  - "День 3: пошутил про пару с Наруто"
current_status: "Идёт в Ичираку с Маркусом и Наруто"
critical_knowledge:
  - "Маркус — иномирец, знает о лисе Наруто"
nicknames:
  - "Хината-чан"
last_update: "День 3, обед в Ичираку"
`

func TestLoad_OK(t *testing.T) {
	p, err := Load(fullYAML)
	require.NoError(t, err)
	assert.Equal(t, "Хината Хьюга", p.DisplayName)
	assert.Equal(t, "hinata_hyuga", p.FileSlug)
	assert.Equal(t, "Застенчивая, но решительная в бою", p.Temperament)
	assert.Equal(t, "Восхищается Маркусом, краснеет", p.RelationsGG)
	require.Len(t, p.RelationsNPCs, 2)
	assert.Equal(t, "Наруто", p.RelationsNPCs[0].Target)
	assert.Equal(t, "Сильные чувства, скрывает", p.RelationsNPCs[0].Note)
	assert.Equal(t, "Маркус", p.RelationsNPCs[1].Target)
	assert.Equal(t, "Доверяет", p.RelationsNPCs[1].Note)
	require.Len(t, p.Abilities, 2)
	assert.Equal(t, "Бьякуган — bloodline limit", p.Abilities[0])
	require.Len(t, p.PersonalMemory, 2)
	assert.Equal(t, "День 1: познакомилась с Маркусом на поляне", p.PersonalMemory[0])
	assert.Equal(t, "Идёт в Ичираку с Маркусом и Наруто", p.CurrentStatus)
	require.Len(t, p.CriticalKnowledge, 1)
	assert.Equal(t, "Маркус — иномирец, знает о лисе Наруто", p.CriticalKnowledge[0])
	require.Len(t, p.Nicknames, 1)
	assert.Equal(t, "Хината-чан", p.Nicknames[0])
	assert.Equal(t, "День 3, обед в Ичираку", p.LastUpdate)
}

func TestLoad_Empty(t *testing.T) {
	_, err := Load("")
	assert.ErrorIs(t, err, ErrNotFound)
	_, err = Load("   \n\t  ")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestLoad_BadYAML(t *testing.T) {
	_, err := Load("display_name: [unclosed")
	assert.Error(t, err)
}

func TestSave_RoundTrip(t *testing.T) {
	p, err := Load(fullYAML)
	require.NoError(t, err)
	out, err := p.Save()
	require.NoError(t, err)
	p2, err := Load(out)
	require.NoError(t, err)
	assert.Equal(t, p, p2)
}

func TestBuildMarkdown_AllSections(t *testing.T) {
	p, err := Load(fullYAML)
	require.NoError(t, err)
	md, err := p.BuildMarkdown()
	require.NoError(t, err)
	for _, header := range []string{
		"# Хината Хьюга",
		"## Темперамент",
		"## Отношения с ГГ",
		"## Отношения с другими NPC",
		"## Способности",
		"## Личная память/факты",
		"## Текущий статус",
		"## Критические знания",
		"## Никнеймы",
		"## Последнее обновление",
	} {
		assert.Contains(t, md, header, "missing header %q", header)
	}
	// Sample data.
	assert.Contains(t, md, "Застенчивая")
	assert.Contains(t, md, "Восхищается Маркусом")
	assert.Contains(t, md, "- Наруто: Сильные чувства, скрывает")
	assert.Contains(t, md, "1. День 1: познакомилась с Маркусом на поляне")
	assert.Contains(t, md, "Хината-чан")
}

func TestBuildMarkdown_EmptySectionsOmitted(t *testing.T) {
	p := Profile{
		DisplayName: "Ирука",
		FileSlug:    "iruka",
		Temperament: "Добрый учитель",
	}
	md, err := p.BuildMarkdown()
	require.NoError(t, err)
	assert.Contains(t, md, "# Ирука")
	assert.Contains(t, md, "## Темперамент")
	assert.NotContains(t, md, "## Отношения с ГГ")
	assert.NotContains(t, md, "## Способности")
	assert.NotContains(t, md, "## Личная память")
}

func TestMatchSection(t *testing.T) {
	cases := map[string]SectionKind{
		"темперамент":          SectionTemperament,
		"Темперамент":          SectionTemperament,
		"temperament":          SectionTemperament,
		"RelationsGG":          SectionRelationsGG,
		"отношения с гг":       SectionRelationsGG,
		"Relations_npcs":       SectionRelationsNPCs,
		"npc_relations":        SectionRelationsNPCs,
		"abilities":            SectionAbilities,
		"Способности":          SectionAbilities,
		"personal_memory":      SectionPersonalMemory,
		"Личная память":        SectionPersonalMemory,
		"личная память/факты":  SectionPersonalMemory,
		"status":               SectionCurrentStatus,
		"текущий статус":       SectionCurrentStatus,
		"critical_knowledge":   SectionCriticalKnowledge,
		"критические знания":   SectionCriticalKnowledge,
		"nicknames":            SectionNicknames,
		"Никнеймы":             SectionNicknames,
		"last_update":          SectionLastUpdate,
		"последнее обновление": SectionLastUpdate,
		"unknown_section":      SectionUnknown,
		"":                     SectionUnknown,
	}
	for in, want := range cases {
		assert.Equal(t, want, MatchSection(in), "input=%q", in)
	}
}

func TestUpdateSection_Replace(t *testing.T) {
	p, _ := Load(fullYAML)
	changed := p.UpdateSection(SectionTemperament, "Другая черта")
	assert.True(t, changed)
	assert.Equal(t, "Другая черта", p.Temperament)
}

func TestUpdateSection_ReplaceSameTextNoop(t *testing.T) {
	p, _ := Load(fullYAML)
	changed := p.UpdateSection(SectionTemperament, p.Temperament)
	assert.False(t, changed)
}

func TestUpdateSection_EmptyTextNoop(t *testing.T) {
	p, _ := Load(fullYAML)
	changed := p.UpdateSection(SectionTemperament, "   ")
	assert.False(t, changed)
	assert.Equal(t, "Застенчивая, но решительная в бою", p.Temperament)
}

func TestUpdateSection_AppendAbility(t *testing.T) {
	p, _ := Load(fullYAML)
	changed := p.UpdateSection(SectionAbilities, "Илливой техника")
	assert.True(t, changed)
	assert.Contains(t, p.Abilities, "Илливой техника")
}

func TestUpdateSection_DedupAbility(t *testing.T) {
	p, _ := Load(fullYAML)
	before := len(p.Abilities)
	changed := p.UpdateSection(SectionAbilities, p.Abilities[0])
	assert.False(t, changed)
	assert.Equal(t, before, len(p.Abilities))
}

func TestUpdateSection_AppendPersonalMemory(t *testing.T) {
	p, _ := Load(fullYAML)
	changed := p.UpdateSection(SectionPersonalMemory, "День 4: новый факт")
	assert.True(t, changed)
	assert.Equal(t, "День 4: новый факт", p.PersonalMemory[len(p.PersonalMemory)-1])
}

func TestUpdateSection_PersonalMemoryCaseInsensitiveDedup(t *testing.T) {
	p, _ := Load(fullYAML)
	before := len(p.PersonalMemory)
	changed := p.UpdateSection(SectionPersonalMemory, strings.ToUpper(p.PersonalMemory[0]))
	assert.False(t, changed)
	assert.Equal(t, before, len(p.PersonalMemory))
}

func TestUpdateSection_AppendRelation(t *testing.T) {
	p, _ := Load(fullYAML)
	changed := p.UpdateSection(SectionRelationsNPCs, "Саске: неприязнь")
	assert.True(t, changed)
	require.Len(t, p.RelationsNPCs, 3)
	assert.Equal(t, "Саске", p.RelationsNPCs[2].Target)
	assert.Equal(t, "неприязнь", p.RelationsNPCs[2].Note)
}

func TestUpdateSection_ReplaceExistingRelation(t *testing.T) {
	p, _ := Load(fullYAML)
	changed := p.UpdateSection(SectionRelationsNPCs, "Наруто: помирились, отклонение от канона")
	assert.True(t, changed)
	require.Len(t, p.RelationsNPCs, 2)
	// Same target, new note.
	assert.Equal(t, "Наруто", p.RelationsNPCs[0].Target)
	assert.Equal(t, "помирились, отклонение от канона", p.RelationsNPCs[0].Note)
}

func TestUpdateSection_RelationNoColon(t *testing.T) {
	p := Profile{DisplayName: "X"}
	changed := p.UpdateSection(SectionRelationsNPCs, "Саске")
	assert.True(t, changed)
	require.Len(t, p.RelationsNPCs, 1)
	assert.Equal(t, "Саске", p.RelationsNPCs[0].Target)
	assert.Equal(t, "", p.RelationsNPCs[0].Note)
}

func TestUpdateSection_AppendNickname(t *testing.T) {
	p, _ := Load(fullYAML)
	changed := p.UpdateSection(SectionNicknames, "Хината-сама")
	assert.True(t, changed)
	assert.Contains(t, p.Nicknames, "Хината-сама")
}

func TestUpdateSection_AppendCriticalKnowledge(t *testing.T) {
	p, _ := Load(fullYAML)
	changed := p.UpdateSection(SectionCriticalKnowledge, "Тайна клана Хьюга")
	assert.True(t, changed)
	assert.Contains(t, p.CriticalKnowledge, "Тайна клана Хьюга")
}

func TestUpdateSection_LastUpdateReplace(t *testing.T) {
	p, _ := Load(fullYAML)
	changed := p.UpdateSection(SectionLastUpdate, "День 5, новая сцена")
	assert.True(t, changed)
	assert.Equal(t, "День 5, новая сцена", p.LastUpdate)
}

func TestUpdateSection_UnknownSectionNoop(t *testing.T) {
	p, _ := Load(fullYAML)
	changed := p.UpdateSection(SectionUnknown, "anything")
	assert.False(t, changed)
}

func TestMigrateFromMarkdown_OK(t *testing.T) {
	body := `# Хината Хьюга

## Темперамент
Застенчивая, но решительная в бою

## Отношения с ГГ
Восхищается Маркусом, краснеет

## Отношения с другими NPC
- Наруто: Сильные чувства, скрывает
- Маркус: Доверяет

## Способности
- Бьякуган — bloodline limit
- Базовые техники Хьюга

## Личная память/факты
- День 1: познакомилась с Маркусом на поляне
- День 3: пошутил про пару с Наруто

## Текущий статус
Идёт в Ичираку с Маркусом и Наруто

## Критические знания
- Маркус — иномирец, знает о лисе Наруто

## Никнеймы
- Хината-чан

## Последнее обновление
День 3, обед в Ичираку
`
	p, err := MigrateFromMarkdown(body, "hinata_hyuga")
	require.NoError(t, err)
	assert.Equal(t, "Хината Хьюга", p.DisplayName)
	assert.Equal(t, "Застенчивая, но решительная в бою", p.Temperament)
	assert.Equal(t, "Восхищается Маркусом, краснеет", p.RelationsGG)
	require.Len(t, p.RelationsNPCs, 2)
	assert.Equal(t, "Наруто", p.RelationsNPCs[0].Target)
	assert.Equal(t, "Сильные чувства, скрывает", p.RelationsNPCs[0].Note)
	require.Len(t, p.Abilities, 2)
	require.Len(t, p.PersonalMemory, 2)
	assert.Equal(t, "Идёт в Ичираку с Маркусом и Наруто", p.CurrentStatus)
	require.Len(t, p.CriticalKnowledge, 1)
	require.Len(t, p.Nicknames, 1)
	assert.Equal(t, "День 3, обед в Ичираку", p.LastUpdate)
}

func TestMigrateFromMarkdown_Empty(t *testing.T) {
	_, err := MigrateFromMarkdown("", "slug")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestMigrateFromMarkdown_Minimal(t *testing.T) {
	body := "# Ирука"
	p, err := MigrateFromMarkdown(body, "iruka")
	require.NoError(t, err)
	assert.Equal(t, "Ирука", p.DisplayName)
	assert.Equal(t, "iruka", p.FileSlug)
	assert.Empty(t, p.Abilities)
	assert.Empty(t, p.PersonalMemory)
}

func TestMigrateFromMarkdown_NumberedList(t *testing.T) {
	body := `# X

## Личная память/факты
1. Первый факт
2. Второй факт
3. Третий факт
`
	p, err := MigrateFromMarkdown(body, "x")
	require.NoError(t, err)
	require.Len(t, p.PersonalMemory, 3)
	assert.Equal(t, "Первый факт", p.PersonalMemory[0])
	assert.Equal(t, "Третий факт", p.PersonalMemory[2])
}

func TestSortedKeys(t *testing.T) {
	p, _ := Load(fullYAML)
	keys := p.SortedKeys()
	// All 9 sections populated.
	assert.Len(t, keys, 9)
	// Alphabetical.
	for i := 1; i < len(keys); i++ {
		assert.True(t, keys[i-1] < keys[i], "not sorted: %v", keys)
	}
}

func TestSortedKeys_Partial(t *testing.T) {
	p := Profile{DisplayName: "X", Temperament: "Y"}
	keys := p.SortedKeys()
	assert.Equal(t, []string{"Темперамент"}, keys)
}

func TestNPCPersonalMemoryLimit(t *testing.T) {
	// Sanity: the constant is what the dispatcher /
	// summarizer will compare against. If we change
	// it we need to update both call sites in
	// sync, so we assert the value here.
	assert.Equal(t, 25, NPCPersonalMemoryLimit)
}

// TestProfile_BuildCompact: the compact LOD drops the
// big arrays (abilities / personal_memory /
// critical_knowledge / relations_npcs) and keeps the
// "what is this NPC" essentials. Operators rely on
// this to keep the world block under the cache budget
// when the cast grows past 4-5 NPCs.
func TestProfile_BuildCompact(t *testing.T) {
	p, err := Load(`display_name: "Какаши"
file_slug: "kakashi"
temperament: "хладнокровный, методичный"
relations_gg: "наставник ГГ, уважает"
current_status: "на тренировочной площадке"
abilities:
  - Катон
  - Шаринган
personal_memory:
  - 1
  - 2
  - 3
critical_knowledge:
  - знает о тайне ГГ
relations_npcs:
  - target: Наруто
    note: ученик
last_update: "тренировал ГГ"
`)
	require.NoError(t, err)

	compact := p.BuildCompact()
	assert.Contains(t, compact, "## Какаши")
	assert.Contains(t, compact, "Темперамент: хладнокровный, методичный")
	assert.Contains(t, compact, "К ГГ: наставник ГГ, уважает")
	assert.Contains(t, compact, "Текущий статус: на тренировочной площадке")
	assert.Contains(t, compact, "Свежее: тренировал ГГ")
	// Big arrays are dropped.
	assert.NotContains(t, compact, "abilities", "compact LOD drops abilities")
	assert.NotContains(t, compact, "personal_memory", "compact LOD drops personal_memory")
	assert.NotContains(t, compact, "critical_knowledge", "compact LOD drops critical_knowledge")
	// relations_npcs note is dropped; only the target
	// name is listed ("Связи: Наруто"). The full
	// "note: ученик" is in the YAML and accessible
	// through search_npc.
	assert.NotContains(t, compact, "ученик",
		"compact LOD drops relations_npcs note, keeps only the target name")
}

// TestProfile_BuildOneLine: the one-line LOD is a
// single short paragraph — used for background NPCs
// in 10+ scenes where even the compact form is too
// expensive.
func TestProfile_BuildOneLine(t *testing.T) {
	p, err := Load(`display_name: "Хината"
file_slug: "hinata"
temperament: "застенчивая, добрая"
current_status: "тренируется у Какаши"
personal_memory:
  - a
  - b
`)
	require.NoError(t, err)

	one := p.BuildOneLine()
	assert.Contains(t, one, "Хината")
	assert.Contains(t, one, "застенчивая, добрая")
	assert.Contains(t, one, "Сейчас: тренируется у Какаши")
	// Compact details are dropped.
	assert.NotContains(t, one, "personal_memory")
	// No markdown ## headers (the whole point is
	// minimum render cost).
	assert.NotContains(t, one, "##")
}

// TestProfile_BuildCompact_EmptyFieldsDropped: missing
// fields are silently dropped (no "Темперамент:" line
// when temperament is empty). The model never sees
// empty placeholders.
func TestProfile_BuildCompact_EmptyFieldsDropped(t *testing.T) {
	p, err := Load(`display_name: "Безликий"
file_slug: "x"
`)
	require.NoError(t, err)

	compact := p.BuildCompact()
	assert.Contains(t, compact, "## Безликий")
	assert.NotContains(t, compact, "Темперамент:")
	assert.NotContains(t, compact, "К ГГ:")
	assert.NotContains(t, compact, "Текущий статус:")
}

// TestProfile_BuildOneLine_TruncatesLongFields: a 200-rune
// status is cut at 120 runes with an ellipsis so
// the one-line LOD stays bounded regardless of how
// much prose the operator or model crammed into
// the field.
func TestProfile_BuildOneLine_TruncatesLongFields(t *testing.T) {
	long := strings.Repeat("X", 200)
	p, err := Load("display_name: \"Long\"\nfile_slug: \"x\"\ntemperament: \"" + long + "\"\ncurrent_status: \"" + long + "\"\n")
	require.NoError(t, err)

	one := p.BuildOneLine()
	// Each field is at most 120 runes + ellipsis suffix.
	// We assert "less than 200 runes per field" — the
	// truncation must be in effect.
	temperamentSection := strings.Split(one, "Сейчас:")[0]
	assert.Less(t, utf8RuneCount(temperamentSection), 200,
		"temperament section must be truncated")
	assert.Contains(t, one, "…", "ellipsis suffix present when truncated")
}

// utf8RuneCount is a stdlib-free rune counter — the
// test asserts on logical characters, not bytes,
// because truncateRune cuts at rune boundaries.
func utf8RuneCount(s string) int {
	return len([]rune(s))
}
