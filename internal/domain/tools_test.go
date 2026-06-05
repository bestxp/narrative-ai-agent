package domain

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
		var probe map[string]any
		require.NoError(t, json.Unmarshal(tl.Function.Parameters, &probe))
		assert.Equal(t, "object", probe["type"])
	}
}

func TestTools_SchemaIsValidObject(t *testing.T) {
	// Every tool must declare a JSON-schema object with at least one
	// property. Required is optional (some tools — e.g. maintenance —
	// have only optional arguments).
	for _, tl := range Tools() {
		var probe struct {
			Type       string         `json:"type"`
			Properties map[string]any `json:"properties"`
		}
		require.NoError(t, json.Unmarshal(tl.Function.Parameters, &probe))
		assert.Equal(t, "object", probe.Type, "tool %s", tl.Function.Name)
		assert.NotEmpty(t, probe.Properties, "tool %s has no properties", tl.Function.Name)
	}
}

func TestEndDayTool_Shape(t *testing.T) {
	for _, tl := range Tools() {
		if tl.Function.Name == "end_day" {
			assert.Contains(t, tl.Function.Description, "memorise")
			var probe struct {
				Properties struct {
					Day     map[string]any `json:"day"`
					Summary map[string]any `json:"summary"`
				} `json:"properties"`
			}
			require.NoError(t, json.Unmarshal(tl.Function.Parameters, &probe))
			assert.Equal(t, "integer", probe.Properties.Day["type"])
			assert.Equal(t, "string", probe.Properties.Summary["type"])
			return
		}
	}
	t.Fatal("end_day tool not found")
}
