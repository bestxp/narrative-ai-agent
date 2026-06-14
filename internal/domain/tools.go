package domain

import (
	"encoding/json"
	"fmt"
	"sort"
)

// Tool declarations follow the OpenAI function-calling schema. The
// parameters block is described as a Go value (Schema) instead of
// an inline JSON literal so the bot can:
//
//   - read the contract as code,
//   - extend it with helper builders (Enum, Required, Min/MaxItems)
//     without writing new JSON literals by hand,
//   - serialize the whole thing at startup once.
//
// Tools are immutable once constructed. The JSON form of Parameters
// is what the LLM client puts on the wire; everything else is for
// humans.

type Tool struct {
	Type     string
	Function ToolFunctionSchema
}

type ToolFunctionSchema struct {
	Name        string
	Description string
	Parameters  Schema
}

// Schema is the typed description of an object schema. It mirrors
// the subset of JSON Schema that the OpenAI /v1/chat/completions
// endpoint understands (the "strict" function-calling subset). All
// fields are exported so tests can introspect them without
// parsing the JSON back.
type Schema struct {
	Type string `json:"type"`
	// Properties is intentionally NOT marked omitempty: the
	// strict subset of JSON Schema that OpenAI and Anthropic
	// use for tool declarations requires the `properties`
	// key to be present even when the value is an empty
	// object (no arguments). Stdlib's json treats len(map)==0
	// as "empty" for omitempty, which would silently drop
	// the key for tools like maintain_lore that have
	// no parameters — and then the strict schema
	// validator on the wire would reject the request with
	// "additionalProperties must be false" (because the
	// implicit `{}` lacks the explicit lock-down).
	Properties           map[string]Schema `json:"properties"`
	Required             []string          `json:"required,omitempty"`
	AdditionalProperties *bool             `json:"additionalProperties,omitempty"`
	Description          string            `json:"description,omitempty"`
	Enum                 []any             `json:"enum,omitempty"`
	Items                *Schema           `json:"items,omitempty"`
	MinItems             *int              `json:"minItems,omitempty"`
	MaxItems             *int              `json:"maxItems,omitempty"`
}

// Object starts a new object schema. Pass props as a list of
// (name, schema) pairs in declaration order; the same names get
// picked up by Required automatically. Use Optional for properties
// that should not be added to the required list.
func Object(props ...Property) Schema {
	out := Schema{Type: "object", Properties: map[string]Schema{}}
	for _, p := range props {
		out.Properties[p.Name] = p.Schema
	}
	required := make([]string, 0, len(props))
	for _, p := range props {
		if p.Required {
			required = append(required, p.Name)
		}
	}
	if len(required) > 0 {
		sort.Strings(required) // stable wire format regardless of declaration order
		out.Required = required
	}
	if len(props) > 0 {
		// additionalProperties=false is required by the OpenAI strict
		// subset for tool calls.
		f := false
		out.AdditionalProperties = &f
	}
	return out
}

// Property is the (name, schema, required) tuple used by Object.
type Property struct {
	Name     string
	Schema   Schema
	Required bool
}

// Optional returns a non-required property. The second argument is
// the schema; pass it in via the helpers below.
func Optional(name string, s Schema) Property { return Property{Name: name, Schema: s} }

// Required returns a required property.
func Required(name string, s Schema) Property { return Property{Name: name, Schema: s, Required: true} }

// String returns a string schema. The description is the
// human-readable hint the LLM sees in the function-calling prompt.
func String(description string) Schema {
	return Schema{Type: "string", Description: description}
}

// Integer returns an integer schema.
func Integer(description string) Schema {
	return Schema{Type: "integer", Description: description}
}

// Boolean returns a boolean schema.
func Boolean(description string) Schema {
	return Schema{Type: "boolean", Description: description}
}

// StringEnum returns a string schema constrained to a fixed list
// of values. The values appear in the schema verbatim, so the LLM
// sees the exact vocabulary the dispatcher will accept.
func StringEnum(description string, values ...string) Schema {
	enum := make([]any, 0, len(values))
	for _, v := range values {
		enum = append(enum, v)
	}
	return Schema{Type: "string", Description: description, Enum: enum}
}

// ArrayOf returns an array schema whose items match items.
func ArrayOf(items Schema) Schema {
	return Schema{Type: "array", Items: &items}
}

// ArrayOfStrings is shorthand for ArrayOf(String("")).
func ArrayOfStrings(description string) Schema {
	s := ArrayOf(String(""))
	s.Description = description
	return s
}

// ArrayOfStringsBounded is ArrayOfStrings with minItems/maxItems
// constraints, used for the rotate_plan tool where the skill
// mandates 3-5 events.
func ArrayOfStringsBounded(description string, min, max int) Schema {
	s := ArrayOfStrings(description)
	s.MinItems = &min
	s.MaxItems = &max
	return s
}

// BoolPtr is a tiny helper for callers that want to set
// AdditionalProperties manually.
func BoolPtr(b bool) *bool { return &b }

// IntPtr is the int equivalent.
func IntPtr(n int) *int { return &n }

// MarshalParameters renders the schema as the JSON object the
// OpenAI API expects. It is called once per tool at startup; the
// cost is negligible compared to the stream round-trip.
func (t Tool) MarshalParameters() (json.RawMessage, error) {
	out, err := json.Marshal(t.Function.Parameters)
	if err != nil {
		return nil, fmt.Errorf("tool %s: marshal parameters: %w", t.Function.Name, err)
	}
	return out, nil
}

// Tools returns the canonical tool list for the GM. Keep it small —
// every extra tool burns context window and confuses smaller models.
func Tools() []Tool {
	return ProdTools()
}

// ProdTools returns the **8 tools the bot wires on every GM turn**.
// This is the canonical list; both driver implementations
// (internal/adapter/llm/openai and internal/adapter/llm/anthropic)
// and both probes (cmd/test-openapi, cmd/test-anthropic) read from
// here so production and probes exercise the same schemas.
//
// Hardcoded: with h4-by-default config (8 tools, tool_choice=auto,
// response_format=json_object on openai / system-prompt on anthropic,
// strict_tools=true) the list is fixed — no configuration surface
// remains to enable/disable individual tools.
func ProdTools() []Tool {
	return []Tool{
		endDayTool(),
		endSceneTool(),
		updateStateTool(),
		createNpcTool(),
		updateNpcTool(),
		// Character file tools (h5 refactor — split
		// out of the legacy update_character dispatcher).
		updateSoulTool(),
		updateSkillTool(),
		updateMemoryTool(),
		updateInventoryTool(),
		removeInventoryItemTool(),
		setCurrencyTool(),
		removeCurrencyTool(),
		searchNpcTool(),
		maintainLoreTool(),
		rotatePlanTool(),
		// Staging tools (story arcs).
		updateStageTool(),
		advanceTimelineTool(),
	}
}

func endDayTool() Tool {
	return Tool{
		Type: "function",
		Function: ToolFunctionSchema{
			Name:        "end_day",
			Description: "Записать итоги прошедшего дня в **chronicle** и обновить **world state**. Вызывай в конце сцены/дня по правилу MAINTENANCE FIRST.",
			Parameters: Object(
				Required("day", Integer("Номер завершённого дня")),
				Required("summary", String("1-2 предложения сухой выжимки, без диалогов и эмоций")),
			),
		},
	}
}

func endSceneTool() Tool {
	return Tool{
		Type: "function",
		Function: ToolFunctionSchema{
			Name:        "end_scene",
			Description: "Зафиксировать конец текущей сцены: сжать диалог в **world state** («## Хроника сцены Д<N>»), очистить список активных NPC (оставить только тех, кто в permanent party), сбросить историю диалога. Используй когда сцена исчерпана (игрок уходит из локации / переключается на новый сюжет), но день ещё не закончен. Не вызывай в конце дня — для этого есть end_day.",
			Parameters: Object(
				Optional("permanent_party", String("Список имён NPC, которые остаются в активном ростере (через запятую). Если пусто — берётся из «## permanent party» в world state активного мира. Если нигде не указано — ростер не меняется.")),
			),
		},
	}
}

func maintainLoreTool() Tool {
	return Tool{
		Type: "function",
		Function: ToolFunctionSchema{
			Name:        "maintain_lore",
			Description: "Сжать **lore** при > 500 строк. Хронологический порядок + отклонения от канона + смерть NPC + первые появления NPC — сохраняем; повседневные действия / промежуточные эмоции / случайные реплики — удаляем. **Canon** НЕ трогаем (внешний канон).",
			Parameters:  emptyObjectSchema(),
		},
	}
}

// emptyObjectSchema returns the canonical "empty object
// parameters" shape for tools that take no arguments. The
// shape {type: object, properties: {}, additionalProperties:
// false} satisfies the OpenAI/Anthropic strict schema
// requirement (closed object, no extra fields) and round-
// trips through the tool list serialiser. We return a
// Schema value rather than calling Object() with no
// properties because Object() does not set
// additionalProperties when there is nothing to lock down —
// strictly that's still valid JSON Schema, but the strict
// mode on OpenAI rejects it on the wire. We always emit the
// explicit false.
func emptyObjectSchema() Schema {
	return Schema{
		Type:                 "object",
		Properties:           map[string]Schema{},
		AdditionalProperties: BoolPtr(false),
	}
}

func updateStateTool() Tool {
	return Tool{
		Type: "function",
		Function: ToolFunctionSchema{
			Name:        "update_state",
			Description: "Обновить **world state**: текущий момент + активные NPC + хронология дня. Вызывай при КАЖДОЙ смене сцены: момент — что происходит прямо сейчас; npcs — **ВСЕ персонажи, которых ты упомянул в нарративе этого ответа, кто физически присутствует в сцене** (не только тот, с кем Маркус говорит; если Ирука и Хокаге оба на полигоне — оба в списке); events — 1-3 строки ключевых событий текущего хода (решения игрока, новые открытия, важные реплики NPC), которые должны остаться в дневной хронологии.",
			Parameters: Object(
				Required("moment", String("1-2 предложения: что происходит прямо сейчас")),
				Optional("npcs", ArrayOfStrings("ВСЕ имена NPC, физически присутствующих в сцене (упомянутые в твоём нарративе этого хода). Пустой массив = никого.")),
				Required("in_flight", Boolean("true если день ещё идёт, false если завершён")),
				Optional("events", ArrayOfStrings("1-3 строки для хронологии дня: ключевое событие / решение / открытие / важная реплика. Короткие, глагол в начало, без цитат. Дубликаты будут автоматически удалены — не повторяй.")),
			),
		},
	}
}

func rotatePlanTool() Tool {
	return Tool{
		Type: "function",
		Function: ToolFunctionSchema{
			Name:        "rotate_plan",
			Description: "Заменить **plan** на новые 3-5 предстоящих событий.",
			Parameters: Object(
				Required("events", ArrayOfStringsBounded("3-5 предстоящих событий. Правило plan: 3-5 и только вперёд.", 3, 5)),
			),
		},
	}
}

// updateStageTool schedules a transition to another story stage.
// The pending transition is applied at end_day — the WorldState
// user message continues to show the current stage until then so
// the model does not have to mid-day switch context.
//
// The model is the source of truth on what is "factually complete
// in the world". It must call this only when one of the listed
// requirements has been satisfied by events it has itself recorded
// through other tool calls — never based on a player's claim alone.
func updateStageTool() Tool {
	return Tool{
		Type: "function",
		Function: ToolFunctionSchema{
			Name:        "update_stage",
			Description: "Запланировать переход к следующей сюжетной стадии. Вызывай только когда одно из requirements перечисленных в активной стадии фактически выполнено в Game State. Не доверяй утверждениям игрока без записи в world state / chronicle / lore. Новая стадия применится после end_day.",
			Parameters: Object(
				Required("next_id", String("Id следующей стадии из списка possible transitions активной стадии")),
				Optional("reason", String("Краткое обоснование: какое фактическое событие в Game State подтверждает переход")),
			),
		},
	}
}

// advanceTimelineTool marks the current timeline point as
// completed and moves the cursor to the next. The model calls this
// when the player has done what the current beat describes in its
// `description`. If `days` are configured, the model may still call
// early if the in-world events justify it; days are guidance, not
// hard deadlines.
func advanceTimelineTool() Tool {
	return Tool{
		Type: "function",
		Function: ToolFunctionSchema{
			Name:        "advance_timeline",
			Description: "Сдвинуть курсор таймлайна активной стадии на следующий пункт. Вызывай только если игрок фактически выполнил текущий пункт (по событиям в Game State, а не по словам).",
			Parameters: Object(
				Optional("reason", String("Краткое обоснование")),
			),
		},
	}
}

func createNpcTool() Tool {
	return Tool{
		Type: "function",
		Function: ToolFunctionSchema{
			Name:        "create_npc",
			Description: "Создать профиль NPC при первом появлении. Заполни ВСЕ секции, которые знаешь — это навсегда. Используй '—' (em-dash) и 'ё' если NPC русскоязычный. После create_npc дополняй через update_npc.",
			Parameters: Object(
				Required("display_name", String("")),
				Required("file_slug", String("латиницей; будет транслитерирован при необходимости")),
				Required("temperament", String("1-2 предложения: характер, манера поведения")),
				Required("relations", String("Отношения с ГГ: статус, связь, отношение. Можно многострочно.")),
				Required("abilities", ArrayOfStrings("Каждая способность отдельной строкой. Не объединяй несколько в одну — система соберёт массив сама.")),
				Optional("nicknames", ArrayOfStrings("")),
				Optional("personal_memory", String("Личная память: что NPC помнит / знает о ГГ или о мире. Можно многострочно.")),
				Optional("current_status", String("Текущий статус: локация, состояние, активность. Можно многострочно.")),
				Optional("critical_knowledge", String("Критические знания: что NPC знает такого, что другие не знают. Можно многострочно.")),
				Optional("last_update", String("Короткая строка вида 'День N — что произошло'. По умолчанию ставится сегодняшняя дата.")),
			),
		},
	}
}

func worldLeaveTool() Tool {
	return Tool{
		Type: "function",
		Function: ToolFunctionSchema{
			Name:        "leave_world",
			Description: "Переключить активный мир, инициализировать новый при отсутствии.",
			Parameters: Object(
				Required("to_world", String("латинский slug нового мира")),
				Optional("skip_note", String("Сколько прошло времени в старом мире; пусто = мгновение")),
			),
		},
	}
}

// updateSoulTool appends a fact to a section in
// characters/<active>/SOUL.yaml. The file holds
// "who the GG is": identity, age, preferences,
// philosophy, etc. SOUL sections are FREE-FORM —
// the LLM may invent new section names (a
// "Прочее" fallback is implied for unknown
// headings).
//
// h5 refactor: the legacy update_character tool
// dispatched on a `file` argument
// (SOUL/SKILL/memory). The dispatcher was
// stringy and easy to confuse — a single typo
// (`memory` vs `Memory`) silently dropped the
// call. Splitting into one tool per file removes
// the discriminator and gives each file its own
// clean schema.
func updateSoulTool() Tool {
	return Tool{
		Type: "function",
		Function: ToolFunctionSchema{
			Name:        "update_soul",
			Description: "Дописать факт в **soul** активного персонажа — кто ГГ (сущность, философия, предпочтения, легенда). Секции СВОБОДНЫЕ: можно создавать новые ('Легенда', 'Мотивация', что угодно). Один вызов = один факт, не абзац. Без markdown, без маркеров. Секции append-only, дедуп exact-string.",
			Parameters: Object(
				Required("section", String("Заголовок секции (например, 'Истинная сущность', 'Предпочтения', 'Философия и принципы', 'Легенда для прикрытия'). Если секции нет — она создастся.")),
				Required("append", String("Текст для добавления в конец секции. 1 предложение.")),
			),
		},
	}
}

// updateSkillTool appends a fact to a section in
// characters/<active>/skill.yaml. Sections are
// STRICT — must be on charprofile.SkillFixedSections
// (Ранг, Оружие, Базовые способности,
// Фундаментальные стихии, Особые проявления,
// Универсальные навыки, Ограничения, Глаза,
// Доспех). Unknown section names are rejected
// at the dispatcher level so the LLM does not
// silently pollute the file with "Misc"-style
// noise.
func updateSkillTool() Tool {
	canonical := []string{
		"Ранг", "Оружие", "Базовые способности",
		"Фундаментальные стихии", "Особые проявления",
		"Универсальные навыки", "Ограничения", "Глаза", "Доспех",
	}
	return Tool{
		Type: "function",
		Function: ToolFunctionSchema{
			Name:        "update_skill",
			Description: "Дописать факт в **skill** активного персонажа — что ГГ умеет (ранг, оружие, техники, ограничения). Секции СТРОГО по enum: Ранг / Оружие / Базовые способности / Фундаментальные стихии / Особые проявления / Универсальные навыки / Ограничения / Глаза / Доспех. Любая другая секция будет отклонена. Один вызов = один факт. Без markdown, без маркеров.",
			Parameters: Object(
				Required("section", StringEnum("Секция из фиксированного списка (skill — справочник способностей)", canonical...)),
				Required("append", String("Текст для добавления. 1 предложение, БЕЗ markdown.")),
			),
		},
	}
}

// updateMemoryTool appends a fact to a section in
// characters/<active>/memory.yaml — what the GG
// remembers. Sections are STRICT and the
// enumerator is narrower than skill.yaml
// (Яркие моменты / Факты о мире / Обещания и цели
// / Важные люди). Anything else is rejected.
func updateMemoryTool() Tool {
	canonical := []string{
		"Яркие моменты", "Факты о мире", "Обещания и цели", "Важные люди",
	}
	return Tool{
		Type: "function",
		Function: ToolFunctionSchema{
			Name:        "update_memory",
			Description: "Дописать яркое воспоминание / факт в **memory** активного персонажа. Секции СТРОГО: Яркие моменты / Факты о мире / Обещания и цели / Важные люди. ЗАПРЕЩЕНО: даты, 'День N', дублирование world state / chronicle. Только то, что ГГ реально запомнил — эмоционально или фактически. Один вызов = один факт.",
			Parameters: Object(
				Required("section", StringEnum("Секция из фиксированного списка (memory — яркие воспоминания ГГ)", canonical...)),
				Required("append", String("Текст для добавления. 1-2 предложения, без дат, без 'День N'.")),
			),
		},
	}
}

// updateInventoryTool adds (or REPLACES) an item
// in characters/<active>/inventory.yaml. Items
// are identified by `name` (primary key) — same
// name = REPLACE the description/equip/special.
//
// equip=true means the item is currently worn /
// held / active. The model uses this to filter
// "what I'm using right now".
//
// `type` is the canonical item category (see
// charprofile inventory.go for the enum: weapon,
// armor, accessory, consumable, tool, quest,
// document, material, other). 'other' is the
// fallback when nothing fits.
//
// `special` is free-form text — квестовые флаги,
// одноразовость, привязка к душе, скрытность, etc.
// "нет" if no special properties.
func updateInventoryTool() Tool {
	canonical := []string{
		"weapon", "armor", "accessory", "consumable",
		"tool", "quest", "document", "material", "other",
	}
	return Tool{
		Type: "function",
		Function: ToolFunctionSchema{
			Name:        "update_inventory",
			Description: "Добавить или заменить предмет в **inventory** активного персонажа. Идентификация — по name: если предмет с таким name уже есть, его атрибуты (description / equip / special) ОБНОВЛЯЮТСЯ. Для удаления — отдельный remove_inventory_item. Количество кодируется именем (один items[]-элемент на единицу, или 'Кунай x3' одним элементом).",
			Parameters: Object(
				Required("name", String("Уникальное имя предмета. Используется как первичный ключ. Регистрозависимо.")),
				Required("type", StringEnum("Категория из канонического списка", canonical...)),
				Optional("description", String("Литературное описание, до 4 предложений. Что это, как выглядит.")),
				Optional("equip", Boolean("true если предмет сейчас надет / в руках / активен. По умолчанию false.")),
				Optional("special", String("Особые свойства: 'привязан к душе', 'одноразовый', 'квестовый', 'скрытный', 'ядовитый', 'хрупкий', 'нет'. Свободный текст.")),
			),
		},
	}
}

// removeInventoryItemTool deletes a single item
// by name. Distinct from update_inventory so an
// empty call cannot accidentally nuke the
// inventory. The dispatcher returns the same
// charprofile.ErrItemNotFound when the name is
// not present — the LLM sees the error and can
// recover (no-op, or try a different name).
func removeInventoryItemTool() Tool {
	return Tool{
		Type: "function",
		Function: ToolFunctionSchema{
			Name:        "remove_inventory_item",
			Description: "Удалить предмет из **inventory** активного персонажа по name. Возвращает ошибку если такого name нет — не придумывай.",
			Parameters: Object(
				Required("name", String("Имя предмета для удаления. Должно совпадать с существующим (case-sensitive).")),
			),
		},
	}
}

// setCurrencyTool REPLACES the count of a currency
// line. The model sends the absolute new value
// (not a delta) — read the current value from
// /me, do the arithmetic, submit the new number.
// Clamped to [0, 999_999_999] by the dispatcher.
func setCurrencyTool() Tool {
	return Tool{
		Type: "function",
		Function: ToolFunctionSchema{
			Name:        "set_currency",
			Description: "Заменить count валютной строки в **inventory** активного персонажа. Абсолютное значение (не дельта). Если такой валюты ещё нет — создаётся новая строка. Clamp: [0, 999_999_999].",
			Parameters: Object(
				Required("name", String("Название валюты: 'Рё', 'Кредиты империи', 'Золотые', etc.")),
				Required("count", Integer("Абсолютное количество после изменения. Не отрицательное.")),
			),
		},
	}
}

// removeCurrencyTool deletes a currency line.
// Returns charprofile.ErrItemNotFound if the
// currency name is not present.
func removeCurrencyTool() Tool {
	return Tool{
		Type: "function",
		Function: ToolFunctionSchema{
			Name:        "remove_currency",
			Description: "Удалить валютную строку из **inventory** активного персонажа по name. Возвращает ошибку если такой валюты нет.",
			Parameters: Object(
				Required("name", String("Название валюты для удаления.")),
			),
		},
	}
}

func updateNpcTool() Tool {
	canonical := []string{
		"Темперамент", "Отношения с ГГ", "Отношения с другими NPC",
		"Способности", "Личная память/факты", "Текущий статус",
		"Критические знания", "Последнее обновление",
	}
	return Tool{
		Type: "function",
		Function: ToolFunctionSchema{
			Name:        "update_npc",
			Description: "Дописать новый факт в профиль уже существующего NPC. Вызывай КАЖДЫЙ раз, когда NPC сказал/показал/узнал что-то новое: новый навык, изменение статуса, новый факт о ГГ, новые отношения с другим NPC, новая локация, новое критическое знание. Секции, указанные в schema, дополняются (append). Секция «Последнее обновление» REPLACE — там всегда только самый свежий факт. Один вызов = один короткий факт, не абзац. Не вызывай если NPC ещё не создан — сначала create_npc.",
			Parameters: Object(
				Required("npc", String("Display name NPC (как в world). Например: 'Хокаге', 'Ирука-сенсей', 'Наруто'.")),
				Required("section", StringEnum("В какую секцию профиля писать (одна из канонических)", canonical...)),
				Required("append", String("Короткий факт для дописи. 1 предложение, БЕЗ markdown, БЕЗ маркеров.")),
			),
		},
	}
}

func searchNpcTool() Tool {
	return Tool{
		Type: "function",
		Function: ToolFunctionSchema{
			Name:        "search_npc",
			Description: "Найти NPC по имени / слагу / прозвищу. Используй ТОЛЬКО когда игрок упоминает персонажа, которого НЕТ в активной сцене (его нет в «## Активные NPC» в текущем system message). Возвращает compact-описание (1-2 строки), не полный YAML. Не злоупотребляй — для активных NPC данные уже в контексте.",
			Parameters: Object(
				Required("query", String("Имя, слаg или прозвище NPC. Поиск: точное совпадение по display_name, slug или nickname; substring-фоллбэк если кандидат один.")),
			),
		},
	}
}
