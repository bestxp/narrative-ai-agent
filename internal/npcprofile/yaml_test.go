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
	md := p.BuildMarkdown()
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
	md := p.BuildMarkdown()
	assert.Contains(t, md, "# Ирука")
	assert.Contains(t, md, "## Темперамент")
	assert.NotContains(t, md, "## Отношения с ГГ")
	assert.NotContains(t, md, "## Способности")
	assert.NotContains(t, md, "## Личная память")
}

func TestMatchSection(t *testing.T) {
	cases := map[string]SectionKind{
		"темперамент":         SectionTemperament,
		"Темперамент":         SectionTemperament,
		"temperament":         SectionTemperament,
		"RelationsGG":         SectionRelationsGG,
		"отношения с гг":      SectionRelationsGG,
		"Relations_npcs":      SectionRelationsNPCs,
		"npc_relations":       SectionRelationsNPCs,
		"abilities":           SectionAbilities,
		"Способности":         SectionAbilities,
		"personal_memory":     SectionPersonalMemory,
		"Личная память":       SectionPersonalMemory,
		"личная память/факты": SectionPersonalMemory,
		"status":              SectionCurrentStatus,
		"текущий статус":      SectionCurrentStatus,
		"critical_knowledge":  SectionCriticalKnowledge,
		"критические знания":  SectionCriticalKnowledge,
		"nicknames":           SectionNicknames,
		"Никнеймы":            SectionNicknames,
		"last_update":         SectionLastUpdate,
		"последнее обновление": SectionLastUpdate,
		"unknown_section":     SectionUnknown,
		"":                    SectionUnknown,
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
	assert.Equal(t, 40, NPCPersonalMemoryLimit)
}
