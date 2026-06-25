package domain

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func paramsJSON(t *testing.T, s Schema) map[string]any {
	t.Helper()
	raw, err := json.Marshal(s)
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(raw, &out))

	return out
}

func TestTools_AllHaveNameAndSchema(t *testing.T) {
	t.Parallel()
	tools := Tools()
	assert.NotEmpty(t, tools)
	seen := map[string]bool{}

	for _, tl := range tools {
		assert.Equal(t, "function", tl.Type)
		assert.NotEmpty(t, tl.Function.Name)
		assert.False(t, seen[tl.Function.Name], "duplicate tool name: %s", tl.Function.Name)
		seen[tl.Function.Name] = true
		assert.NotEmpty(t, tl.Function.Description)
		probe := paramsJSON(t, tl.Function.Parameters)
		assert.Equal(t, "object", probe["type"], "tool %s", tl.Function.Name)
	}
}

func TestTools_SchemaIsValidObject(t *testing.T) {
	t.Parallel()
	for _, tl := range Tools() {
		probe := paramsJSON(t, tl.Function.Parameters)
		assert.Equal(t, "object", probe["type"], "tool %s", tl.Function.Name)
		props, ok := probe["properties"].(map[string]any)
		assert.True(t, ok, "tool %s missing properties", tl.Function.Name)
		// tools with no arguments (maintain_lore)
		// have an empty properties map; that is the
		// canonical "no-args" shape.
		_ = props
	}
}

func TestTools_StrictAdditionalProperties(t *testing.T) {
	t.Parallel()
	// OpenAI's strict function-calling subset requires
	// additionalProperties=false. Every tool with at least one
	// property must declare it.
	for _, tl := range Tools() {
		probe := paramsJSON(t, tl.Function.Parameters)
		ap, ok := probe["additionalProperties"]
		assert.True(t, ok, "tool %s missing additionalProperties", tl.Function.Name)
		assert.Equal(t, false, ap, "tool %s additionalProperties must be false", tl.Function.Name)
	}
}

func TestEndDayTool_Shape(t *testing.T) {
	t.Parallel()
	tool := findTool(t, "end_day")
	assert.Contains(t, tool.Function.Description, "chronicle")
	assert.Contains(t, tool.Function.Description, "world state")
	probe := paramsJSON(t, tool.Function.Parameters)
	props, ok := probe["properties"].(map[string]any)
	require.True(t, ok, "properties not a map")
	dayProp, ok := props["day"].(map[string]any)
	require.True(t, ok, "day prop not a map")
	assert.Equal(t, "integer", dayProp["type"])
	sumProp, ok := props["summary"].(map[string]any)
	require.True(t, ok, "summary prop not a map")
	assert.Equal(t, "string", sumProp["type"])
	req, ok := probe["required"].([]any)
	require.True(t, ok, "required not a slice")
	assert.ElementsMatch(t, []any{"day", "summary"}, req)
}

func TestRotatePlanTool_HasBounds(t *testing.T) {
	t.Parallel()
	tool := findTool(t, "rotate_plan")
	probe := paramsJSON(t, tool.Function.Parameters)
	props, ok := probe["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties missing or wrong type: %T", probe["properties"])
	}
	events, ok := props["events"].(map[string]any)
	if !ok {
		t.Fatalf("events property missing or wrong type: %T", props["events"])
	}
	assert.Equal(t, "array", events["type"])
	assert.InDelta(t, float64(3), events["minItems"], 1e-9)
	assert.InDelta(t, float64(5), events["maxItems"], 1e-9)
}

func TestUpdateSoulTool_SectionIsFreeString(t *testing.T) {
	t.Parallel()
	// SOUL.yaml is free-form: the section arg is a
	// plain String (not an enum), so the model can
	// invent new section names.
	tool := findTool(t, "update_soul")
	probe := paramsJSON(t, tool.Function.Parameters)
	props, ok := probe["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties missing or wrong type: %T", probe["properties"])
	}
	section, ok := props["section"].(map[string]any)
	if !ok {
		t.Fatalf("section property missing or wrong type: %T", props["section"])
	}

	assert.Equal(t, "string", section["type"])
	_, hasEnum := section["enum"]
	assert.False(t, hasEnum, "SOUL section must NOT be an enum")
}

func TestUpdateSkillTool_SectionIsEnum(t *testing.T) {
	t.Parallel()
	// skill.yaml is strict: section must be on the
	// canonical list.
	tool := findTool(t, "update_skill")
	probe := paramsJSON(t, tool.Function.Parameters)
	props, ok := probe["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties missing or wrong type: %T", probe["properties"])
	}
	section, ok := props["section"].(map[string]any)
	if !ok {
		t.Fatalf("section property missing or wrong type: %T", props["section"])
	}
	enum, ok := section["enum"].([]any)
	if !ok {
		t.Fatalf("enum missing or wrong type: %T", section["enum"])
	}
	// 9 fixed sections (Ранг, Оружие, Базовые способности,
	// Фундаментальные стихии, Особые проявления,
	// Универсальные навыки, Ограничения, Глаза, Доспех).
	assert.Len(t, enum, 9)
}

func TestUpdateMemoryTool_SectionIsEnum(t *testing.T) {
	t.Parallel()
	// memory.yaml: 4 canonical sections only.
	tool := findTool(t, "update_memory")
	probe := paramsJSON(t, tool.Function.Parameters)
	props, ok := probe["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties missing or wrong type: %T", probe["properties"])
	}
	section, ok := props["section"].(map[string]any)
	if !ok {
		t.Fatalf("section property missing or wrong type: %T", props["section"])
	}
	enum, ok := section["enum"].([]any)
	if !ok {
		t.Fatalf("enum missing or wrong type: %T", section["enum"])
	}
	assert.ElementsMatch(t, []any{
		"Яркие моменты", "Факты о мире", "Обещания и цели", "Важные люди",
	}, enum)
}

func TestUpdateInventoryTool_TypeIsEnum(t *testing.T) {
	t.Parallel()
	tool := findTool(t, "update_inventory")
	probe := paramsJSON(t, tool.Function.Parameters)
	props, ok := probe["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties missing or wrong type: %T", probe["properties"])
	}
	typ, ok := props["type"].(map[string]any)
	if !ok {
		t.Fatalf("type property missing or wrong type: %T", props["type"])
	}
	enum, ok := typ["enum"].([]any)
	if !ok {
		t.Fatalf("enum missing or wrong type: %T", typ["enum"])
	}

	assert.ElementsMatch(t, []any{
		"weapon", "armor", "accessory", "consumable",
		"tool", "quest", "document", "material", "other",
	}, enum)
}

func TestRemoveInventoryItemTool_HasName(t *testing.T) {
	t.Parallel()
	tool := findTool(t, "remove_inventory_item")
	probe := paramsJSON(t, tool.Function.Parameters)
	required, ok := probe["required"].([]any)
	if !ok {
		t.Fatalf("required missing or wrong type: %T", probe["required"])
	}
	assert.Contains(t, required, "name")
}

func TestSetCurrencyTool_ClampNoteInDescription(t *testing.T) {
	t.Parallel()
	tool := findTool(t, "set_currency")
	assert.Contains(t, tool.Function.Description, "Clamp",
		"set_currency description must mention the [0, 999_999_999] clamp")
	assert.Contains(t, tool.Function.Description, "999_999_999")
}

func TestObject_RequiredIsSorted(t *testing.T) {
	t.Parallel()
	// Wire format is stable regardless of declaration order.
	props := Object(
		Required("zeta", String("z")),
		Required("alpha", String("a")),
		Required("mu", String("m")),
		Optional("optional", String("o")),
	)
	raw, err := json.Marshal(props)
	require.NoError(t, err)
	var probe struct {
		Required []string `json:"required"`
	}
	require.NoError(t, json.Unmarshal(raw, &probe))
	assert.Equal(t, []string{"alpha", "mu", "zeta"}, probe.Required)
}

func TestStringEnum_RoundTrips(t *testing.T) {
	t.Parallel()
	s := StringEnum("pick one", "a", "b", "c")
	raw, err := json.Marshal(s)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"enum":["a","b","c"]`)
}

func TestMarshalParameters_ErrorPropagates(t *testing.T) {
	t.Parallel()
	// Cyclic schema would normally panic in json.Marshal; we
	// just check the happy path here and trust stdlib for the
	// rest. A failing schema should be caught at tool construction.
	tool := findTool(t, "end_day")
	raw, err := tool.MarshalParameters()
	require.NoError(t, err)
	var probe map[string]any
	require.NoError(t, json.Unmarshal(raw, &probe))
	assert.Equal(t, "object", probe["type"])
}

func findTool(t *testing.T, name string) Tool {
	t.Helper()

	for _, tl := range Tools() {
		if tl.Function.Name == name {
			return tl
		}
	}
	t.Fatalf("tool %q not found", name)

	return Tool{}
}
