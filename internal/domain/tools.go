package domain

import "encoding/json"

// Tool declarations follow the OpenAI function-calling schema. They
// describe what the GM is allowed to ask the system to do during a
// turn; the LLM picks one and the orchestrator in usecase/gm.go
// dispatches to the matching usecase.

// Tool is the public-facing type the LLM client receives. Internally
// each tool is just a JSON object with `type: "function"` and a
// `function` payload.
type Tool struct {
	Type     string             `json:"type"`
	Function ToolFunctionSchema `json:"function"`
}

type ToolFunctionSchema struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
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
	}
}

func endDayTool() Tool {
	desc := "Записать итоги прошедшего дня в memorise.md и обновить state.md. Вызывай в конце сцены/дня по правилу MAINTENANCE FIRST."
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"day":      {"type": "integer", "description": "Номер завершённого дня"},
			"summary":  {"type": "string", "description": "1-2 предложения сухой выжимки, без диалогов и эмоций"}
		},
		"required": ["day", "summary"],
		"additionalProperties": false
	}`)
	return Tool{Type: "function", Function: ToolFunctionSchema{Name: "end_day", Description: desc, Parameters: schema}}
}

func maintenanceTool() Tool {
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"force": {"type": "boolean", "description": "Прогнать выжимку NPC даже если файлы < 40 строк", "default": false}
		},
		"additionalProperties": false
	}`)
	return Tool{Type: "function", Function: ToolFunctionSchema{Name: "run_maintenance", Description: "Сжать NPC-файлы > 40 строк и закоммитить.", Parameters: schema}}
}

func updateStateTool() Tool {
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"moment": {"type": "string", "description": "1-2 предложения: что происходит прямо сейчас"},
			"npcs":   {"type": "array", "items": {"type": "string"}, "description": "Имена NPC, активных в сцене"},
			"in_flight": {"type": "boolean", "description": "true если день ещё идёт, false если завершён"}
		},
		"required": ["moment", "in_flight"],
		"additionalProperties": false
	}`)
	return Tool{Type: "function", Function: ToolFunctionSchema{Name: "update_state", Description: "Обновить state.md: текущий момент и активные NPC.", Parameters: schema}}
}

func rotatePlanTool() Tool {
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"events": {
				"type": "array",
				"items": {"type": "string"},
				"minItems": 3,
				"maxItems": 5,
				"description": "3-5 предстоящих событий. Правило plan.md: 3-5 и только вперёд."
			}
		},
		"required": ["events"],
		"additionalProperties": false
	}`)
	return Tool{Type: "function", Function: ToolFunctionSchema{Name: "rotate_plan", Description: "Заменить plan.md на новые 3-5 предстоящих событий.", Parameters: schema}}
}

func npcCreateTool() Tool {
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"display_name": {"type": "string"},
			"file_slug":    {"type": "string", "description": "латиницей; будет транслитерирован при необходимости"},
			"temperament":  {"type": "string"},
			"relations":    {"type": "string"},
			"abilities":    {"type": "string"},
			"nicknames":    {"type": "array", "items": {"type": "string"}}
		},
		"required": ["display_name", "file_slug", "temperament", "relations", "abilities"],
		"additionalProperties": false
	}`)
	return Tool{Type: "function", Function: ToolFunctionSchema{Name: "create_npc", Description: "Создать профиль NPC при первом появлении.", Parameters: schema}}
}

func worldLeaveTool() Tool {
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"to_world":     {"type": "string", "description": "латинский slug нового мира"},
			"skip_note":    {"type": "string", "description": "Сколько прошло времени в старом мире; пусто = мгновение"}
		},
		"required": ["to_world"],
		"additionalProperties": false
	}`)
	return Tool{Type: "function", Function: ToolFunctionSchema{Name: "leave_world", Description: "Переключить активный мир, инициализировать новый при отсутствии.", Parameters: schema}}
}
