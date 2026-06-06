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
	Type                 string             `json:"type"`
	Properties           map[string]Schema  `json:"properties,omitempty"`
	Required             []string           `json:"required,omitempty"`
	AdditionalProperties *bool              `json:"additionalProperties,omitempty"`
	Description          string             `json:"description,omitempty"`
	Enum                 []any              `json:"enum,omitempty"`
	Items                *Schema            `json:"items,omitempty"`
	MinItems             *int               `json:"minItems,omitempty"`
	MaxItems             *int               `json:"maxItems,omitempty"`
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
	return []Tool{
		endDayTool(),
		maintenanceTool(),
		updateStateTool(),
		rotatePlanTool(),
		npcCreateTool(),
		worldLeaveTool(),
		characterUpdateTool(),
	}
}

func endDayTool() Tool {
	return Tool{
		Type: "function",
		Function: ToolFunctionSchema{
			Name:        "end_day",
			Description: "Записать итоги прошедшего дня в memorise.md и обновить state.md. Вызывай в конце сцены/дня по правилу MAINTENANCE FIRST.",
			Parameters: Object(
				Required("day", Integer("Номер завершённого дня")),
				Required("summary", String("1-2 предложения сухой выжимки, без диалогов и эмоций")),
			),
		},
	}
}

func maintenanceTool() Tool {
	return Tool{
		Type: "function",
		Function: ToolFunctionSchema{
			Name:        "run_maintenance",
			Description: "Сжать NPC-файлы > 40 строк и закоммитить.",
			Parameters: Object(
				Optional("force", Boolean("Прогнать выжимку NPC даже если файлы < 40 строк")),
			),
		},
	}
}

func updateStateTool() Tool {
	return Tool{
		Type: "function",
		Function: ToolFunctionSchema{
			Name:        "update_state",
			Description: "Обновить state.md: текущий момент, активные NPC и хронологию дня. Вызывай при КАЖДОЙ смене сцены: момент — что происходит прямо сейчас; npcs — кто присутствует; events — 1-3 строки ключевых событий текущего хода (решения игрока, новые открытия, важные реплики NPC), которые должны остаться в дневной хронологии.",
			Parameters: Object(
				Required("moment", String("1-2 предложения: что происходит прямо сейчас")),
				Optional("npcs", ArrayOfStrings("Имена NPC, активных в сцене")),
				Required("in_flight", Boolean("true если день ещё идёт, false если завершён")),
				Optional("events", ArrayOfStrings("1-3 строки для хронологии дня: ключевое событие / решение / открытие / важная реплика. Короткие, глагол в начало, без цитат целиком. Пример: 'Встретил Ируку-сенсея в столовой'.")),
			),
		},
	}
}

func rotatePlanTool() Tool {
	return Tool{
		Type: "function",
		Function: ToolFunctionSchema{
			Name:        "rotate_plan",
			Description: "Заменить plan.md на новые 3-5 предстоящих событий.",
			Parameters: Object(
				Required("events", ArrayOfStringsBounded("3-5 предстоящих событий. Правило plan.md: 3-5 и только вперёд.", 3, 5)),
			),
		},
	}
}

func npcCreateTool() Tool {
	return Tool{
		Type: "function",
		Function: ToolFunctionSchema{
			Name:        "create_npc",
			Description: "Создать профиль NPC при первом появлении.",
			Parameters: Object(
				Required("display_name", String("")),
				Required("file_slug", String("латиницей; будет транслитерирован при необходимости")),
				Required("temperament", String("")),
				Required("relations", String("")),
				Required("abilities", String("")),
				Optional("nicknames", ArrayOfStrings("")),
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

func characterUpdateTool() Tool {
	return Tool{
		Type: "function",
		Function: ToolFunctionSchema{
			Name:        "update_character",
			Description: "Записать новую информацию о персонаже игрока. Вызывай КАЖДЫЙ раз, когда игрок сообщает новое о себе (имя, возраст, занятие, навык, особенность) — чтобы это не потерялось при следующей сессии.",
			Parameters: Object(
				Required("file", StringEnum("В какой файл писать: SOUL (сущность/предыстория), SKILL (навыки/способности), memory (межмировые воспоминания)", "SOUL", "SKILL", "memory")),
				Required("section", String("Заголовок секции в файле (например, 'Истинная сущность', 'Оружие', 'Базовые способности'). Если секции нет — она создастся.")),
				Required("append", String("Текст для добавления в конец секции. Не переписывай существующее — только дополняй.")),
			),
		},
	}
}
