package usecase

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExtractContextCommands_Markdown_UpdateNpc(t *testing.T) {
	body := `**диалоги и действия**
Хината вздрогнула.

**КОНТЕКСТ И ИЗМЕНЕНИЯ**
⦁ update_npc: Хината — статус: смущена
⦁ update_npc: Наруто — статус: ведёт в Ичираку

**БУДУЩЕЕ**
- Обед`
	cmds := extractContextCommands(body)
	require.Len(t, cmds, 2)
	assert.Equal(t, "update_npc", cmds[0].Kind)
	assert.Equal(t, "Хината", cmds[0].Args["npc"])
	assert.Equal(t, "статус", cmds[0].Args["section"])
	assert.Equal(t, "смущена", cmds[0].Args["append"])
	assert.Equal(t, "Наруто", cmds[1].Args["npc"])
	assert.Equal(t, "статус", cmds[1].Args["section"])
	assert.Equal(t, "ведёт в Ичираку", cmds[1].Args["append"])
}

func TestExtractContextCommands_Markdown_Lore(t *testing.T) {
	body := `**КОНТЕКСТ И ИЗМЕНЕНИЯ**
⦁ lore: День 5 — Саске простил Итахи, они помирились

**БУДУЩЕЕ**
- Тренировка`
	cmds := extractContextCommands(body)
	require.Len(t, cmds, 1)
	assert.Equal(t, "append_lore", cmds[0].Kind)
	assert.Equal(t, "День 5", cmds[0].Args["header"])
	assert.Contains(t, cmds[0].Args["bullet"], "Саске простил Итахи")
}

func TestExtractContextCommands_Markdown_UpdateState(t *testing.T) {
	body := `**КОНТЕКСТ И ИЗМЕНЕНИЯ**
⦁ update_state: moment=у ворот Конохи; npcs=Наруто, Хината; events=вышли к воротам; in_flight=true

**БУДУЩЕЕ**
- Обед`
	cmds := extractContextCommands(body)
	require.Len(t, cmds, 1)
	assert.Equal(t, "update_state", cmds[0].Kind)
	assert.Equal(t, "у ворот Конохи", cmds[0].Args["moment"])
	assert.Equal(t, "true", cmds[0].Args["in_flight"])
}

func TestExtractContextCommands_Markdown_NpcShortForm(t *testing.T) {
	body := `**КОНТЕКСТ И ИЗМЕНЕНИЯ**
⦁ npc: Хината — личная память: оговорилась про «не думала о Наруто»`
	cmds := extractContextCommands(body)
	require.Len(t, cmds, 1)
	assert.Equal(t, "update_npc", cmds[0].Kind)
	assert.Equal(t, "Хината", cmds[0].Args["npc"])
	assert.Equal(t, "личная память", cmds[0].Args["section"])
	assert.Contains(t, cmds[0].Args["append"], "оговорилась")
}

func TestExtractContextCommands_JSON(t *testing.T) {
	// Режим A: JSON-объект с полем context.
	body := `{
  "narration": "Хината покраснела.",
  "context": "⦁ update_npc: Хината — статус: смущена\n⦁ lore: День 5 — Саске простил Итахи",
  "future": "Обед в Ичираку",
  "validation": "ok"
}`
	cmds := extractContextCommands(body)
	require.Len(t, cmds, 2)
	assert.Equal(t, "update_npc", cmds[0].Kind)
	assert.Equal(t, "Хината", cmds[0].Args["npc"])
	assert.Equal(t, "append_lore", cmds[1].Kind)
	assert.Equal(t, "День 5", cmds[1].Args["header"])
}

func TestExtractContextCommands_NoContextBlock(t *testing.T) {
	body := `**диалоги и действия**
Хината вздрогнула, отступила.

**БУДУЩЕЕ**
- Обед`
	cmds := extractContextCommands(body)
	assert.Empty(t, cmds)
}

func TestExtractContextCommands_MultipleBullets(t *testing.T) {
	body := `**КОНТЕКСТ И ИЗМЕНЕНИЯ**
- update_npc: Хината — статус: смущена
• update_npc: Наруто — способности: рамен-ная техника
* lore: День 5 — Хината улыбнулась впервые
> create_npc: display_name=Ирука; file_slug=iruka; temperament=добрый учитель
`
	cmds := extractContextCommands(body)
	assert.Len(t, cmds, 4)
	assert.Equal(t, "update_npc", cmds[0].Kind)
	assert.Equal(t, "update_npc", cmds[1].Kind)
	assert.Equal(t, "append_lore", cmds[2].Kind)
	assert.Equal(t, "create_npc", cmds[3].Kind)
	assert.Equal(t, "Ирука", cmds[3].Args["display_name"])
}

func TestExtractContextCommands_UnknownLineIgnored(t *testing.T) {
	body := `**КОНТЕКСТ И ИЗМЕНЕНИЯ**
⦁ state.md: момент обновлён
⦁ memory.md: добавлена запись
⦁ update_npc: Хината — статус: смущена`
	cmds := extractContextCommands(body)
	// "state.md:" and "memory.md:" are status notes,
	// not tool directives.
	require.Len(t, cmds, 1)
	assert.Equal(t, "update_npc", cmds[0].Kind)
}

func TestExtractContextCommands_QuotedNPCLineNotMistakenForDirective(t *testing.T) {
	body := `**диалоги и действия**
— update_npc: Хината — сказал Наруто
— Тренируйся усерднее

**КОНТЕКСТ И ИЗМЕНЕНИЯ**
⦁ update_npc: Хината — статус: смущена`
	cmds := extractContextCommands(body)
	require.Len(t, cmds, 1)
	assert.Equal(t, "Хината", cmds[0].Args["npc"])
}

func TestParseSemicolonPairs(t *testing.T) {
	tests := []struct {
		in   string
		want map[string]string
	}{
		{"a=1; b=2", map[string]string{"a": "1", "b": "2"}},
		{"a=hello world; b=foo", map[string]string{"a": "hello world", "b": "foo"}},
		{"key=\"quoted value\"", map[string]string{"key": "quoted value"}},
		{"empty=", map[string]string{"empty": ""}},
		{"malformed; a=1", map[string]string{"a": "1"}},
		{"", map[string]string{}},
	}
	for _, tc := range tests {
		got := parseSemicolonPairs(tc.in)
		assert.Equal(t, tc.want, got, "input=%q", tc.in)
	}
}

func TestSplitCSV(t *testing.T) {
	assert.Equal(t, []string{"a", "b", "c"}, splitCSV("a, b, c"))
	assert.Equal(t, []string{"a", "b"}, splitCSV("a;b"))
	assert.Empty(t, splitCSV(""))
	assert.Equal(t, []string{"a"}, splitCSV("a"))
}

func TestParseBoolArg(t *testing.T) {
	cases := map[string]bool{
		"true": true, "yes": true, "1": true, "on": true,
		"false": false, "no": false, "0": false, "": false,
		"True": true, "YES": true, // case-insensitive
	}
	for in, want := range cases {
		assert.Equal(t, want, parseBoolArg(in), "input=%q", in)
	}
}

func TestExtractContextCommands_AppendLoreShortForm(t *testing.T) {
	body := `**КОНТЕКСТ И ИЗМЕНЕНИЯ**
⦁ append_lore: header=День 7, bullet=Маркус приземлился в Конохе`
	cmds := extractContextCommands(body)
	require.Len(t, cmds, 1)
	assert.Equal(t, "append_lore", cmds[0].Kind)
	assert.Equal(t, "День 7", cmds[0].Args["header"])
	assert.Equal(t, "Маркус приземлился в Конохе", cmds[0].Args["bullet"])
}

func TestExtractContextCommands_UpdateCharacter(t *testing.T) {
	body := `**КОНТЕКСТ И ИЗМЕНЕНИЯ**
⦁ update_character: file=SKILL; section=martial_arts; append=киокушинкай, муай-тай`
	cmds := extractContextCommands(body)
	require.Len(t, cmds, 1)
	assert.Equal(t, "update_character", cmds[0].Kind)
	assert.Equal(t, "SKILL", cmds[0].Args["file"])
	assert.Equal(t, "martial_arts", cmds[0].Args["section"])
	assert.Equal(t, "киокушинкай, муай-тай", cmds[0].Args["append"])
}

func TestExtractContextCommands_RawPreserved(t *testing.T) {
	body := `**КОНТЕКСТ И ИЗМЕНЕНИЯ**
⦁ update_npc: Хината — статус: смущена`
	cmds := extractContextCommands(body)
	require.Len(t, cmds, 1)
	assert.Contains(t, cmds[0].Raw, "Хината")
	assert.Contains(t, cmds[0].Raw, "смущена")
}
