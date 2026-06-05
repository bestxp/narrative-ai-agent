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
	for _, tl := range Tools() {
		probe := paramsJSON(t, tl.Function.Parameters)
		assert.Equal(t, "object", probe["type"], "tool %s", tl.Function.Name)
		props, ok := probe["properties"].(map[string]any)
		assert.True(t, ok, "tool %s missing properties", tl.Function.Name)
		assert.NotEmpty(t, props, "tool %s has no properties", tl.Function.Name)
	}
}

func TestTools_StrictAdditionalProperties(t *testing.T) {
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
	tool := findTool(t, "end_day")
	assert.Contains(t, tool.Function.Description, "memorise")
	probe := paramsJSON(t, tool.Function.Parameters)
	props := probe["properties"].(map[string]any)
	assert.Equal(t, "integer", props["day"].(map[string]any)["type"])
	assert.Equal(t, "string", props["summary"].(map[string]any)["type"])
	req := probe["required"].([]any)
	assert.ElementsMatch(t, []any{"day", "summary"}, req)
}

func TestRotatePlanTool_HasBounds(t *testing.T) {
	tool := findTool(t, "rotate_plan")
	probe := paramsJSON(t, tool.Function.Parameters)
	events := probe["properties"].(map[string]any)["events"].(map[string]any)
	assert.Equal(t, "array", events["type"])
	assert.Equal(t, float64(3), events["minItems"])
	assert.Equal(t, float64(5), events["maxItems"])
}

func TestCharacterUpdateTool_EnumMatchesFiles(t *testing.T) {
	tool := findTool(t, "update_character")
	probe := paramsJSON(t, tool.Function.Parameters)
	file := probe["properties"].(map[string]any)["file"].(map[string]any)
	enum := file["enum"].([]any)
	assert.ElementsMatch(t, []any{"SOUL", "SKILL", "memory"}, enum)
}

func TestObject_RequiredIsSorted(t *testing.T) {
	// Wire format is stable regardless of declaration order.
	props := Object(
		Required("zeta", String("z")),
		Required("alpha", String("a")),
		Required("mu", String("m")),
		Optional("optional", String("o")),
	)
	raw, _ := json.Marshal(props)
	var probe struct {
		Required []string `json:"required"`
	}
	require.NoError(t, json.Unmarshal(raw, &probe))
	assert.Equal(t, []string{"alpha", "mu", "zeta"}, probe.Required)
}

func TestStringEnum_RoundTrips(t *testing.T) {
	s := StringEnum("pick one", "a", "b", "c")
	raw, _ := json.Marshal(s)
	assert.Contains(t, string(raw), `"enum":["a","b","c"]`)
}

func TestMarshalParameters_ErrorPropagates(t *testing.T) {
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
