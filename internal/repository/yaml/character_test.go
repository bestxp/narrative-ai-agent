package yaml_test

import (
	"testing"

	"github.com/bestxp/narrative-ai-agent/internal/charprofile"
	"github.com/bestxp/narrative-ai-agent/internal/repository/yaml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func (e *testEnv) newSoulRepo() *yaml.SoulYaml   { return yaml.NewSoulYaml(e.store) }
func (e *testEnv) newSkillRepo() *yaml.SkillYaml { return yaml.NewSkillYaml(e.store) }
func (e *testEnv) newMemoryRepo() *yaml.CharacterMemoryYaml {
	return yaml.NewCharacterMemoryYaml(e.store)
}
func (e *testEnv) newInventoryRepo() *yaml.InventoryYaml { return yaml.NewInventoryYaml(e.store) }

func TestSoulYaml_RoundTrip(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	in := charprofile.Soul{
		Name: "Маркус",
		Soul: "13 лет, попаданец",
		Data: []charprofile.Section{
			{Name: "Истинная сущность", Values: []string{"маг крови"}},
		},
	}
	require.NoError(t, env.newSoulRepo().Save("markus", in))

	out, err := env.newSoulRepo().Load("markus")
	require.NoError(t, err)
	assert.Equal(t, in.Name, out.Name)
	assert.Equal(t, in.Soul, out.Soul)
	require.Len(t, out.Data, 1)
	assert.Equal(t, "Истинная сущность", out.Data[0].Name)
	assert.Equal(t, []string{"маг крови"}, out.Data[0].Values)
}

func TestSoulYaml_AppendSection(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	ok, err := env.newSoulRepo().AppendSection("markus", "Предпочтения", "Ирука-сенсей")
	require.NoError(t, err)
	assert.True(t, ok)
	// Duplicate is a no-op.
	ok, _ = env.newSoulRepo().AppendSection("markus", "Предпочтения", "Ирука-сенсей")
	assert.False(t, ok)
}

func TestSkillYaml_RoundTrip(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	in := charprofile.Skill{}
	in.Data = []charprofile.Section{
		{Name: "Ранг", Values: []string{"генин"}},
	}
	require.NoError(t, env.newSkillRepo().Save("markus", in))

	out, err := env.newSkillRepo().Load("markus")
	require.NoError(t, err)
	require.Len(t, out.Data, 1)
	assert.Equal(t, "Ранг", out.Data[0].Name)
	assert.Equal(t, []string{"генин"}, out.Data[0].Values)
}

func TestCharacterMemoryYaml_RoundTrip(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	in := charprofile.Memory{}
	in.Data = []charprofile.Section{
		{Name: "Яркие моменты", Values: []string{"день 1: встреча с Какаши"}},
	}
	require.NoError(t, env.newMemoryRepo().Save("markus", in))

	out, err := env.newMemoryRepo().Load("markus")
	require.NoError(t, err)
	require.Len(t, out.Data, 1)
	assert.Equal(t, "Яркие моменты", out.Data[0].Name)
}

func TestInventoryYaml_RoundTrip(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	inv := charprofile.Inventory{
		Currency: []charprofile.Currency{
			{Name: "Рё", Count: 5000},
		},
		Items: []charprofile.Item{
			{Name: "Кунай", Description: "стандартный", Equip: true},
		},
	}
	require.NoError(t, env.newInventoryRepo().Save("markus", inv))

	out, err := env.newInventoryRepo().Load("markus")
	require.NoError(t, err)
	require.Len(t, out.Currency, 1)
	assert.Equal(t, "Рё", out.Currency[0].Name)
	assert.Equal(t, 5000, out.Currency[0].Count)
	require.Len(t, out.Items, 1)
	assert.Equal(t, "Кунай", out.Items[0].Name)
}

func TestInventoryYaml_AppendItem(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	ok, err := env.newInventoryRepo().AppendItem("markus", charprofile.Item{
		Name: "Кунай", Description: "стандартный", Equip: true,
	})
	require.NoError(t, err)
	assert.True(t, ok)
	// Duplicate name REPLACES.
	ok, _ = env.newInventoryRepo().AppendItem("markus", charprofile.Item{
		Name: "Кунай", Description: "улучшенный", Equip: false,
	})
	assert.True(t, ok, "replace on same name should return true")

	inv, _ := env.newInventoryRepo().Load("markus")
	require.Len(t, inv.Items, 1)
	assert.Equal(t, "улучшенный", inv.Items[0].Description)
}

func TestInventoryYaml_SetCurrency(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	ok, err := env.newInventoryRepo().SetCurrency("markus", "Рё", 1000)
	require.NoError(t, err)
	assert.True(t, ok)
	ok, _ = env.newInventoryRepo().SetCurrency("markus", "Рё", 2000)
	assert.True(t, ok)

	inv, _ := env.newInventoryRepo().Load("markus")
	require.Len(t, inv.Currency, 1)
	assert.Equal(t, 2000, inv.Currency[0].Count)
}

func TestInventoryYaml_RemoveItem(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	_, err := env.newInventoryRepo().AppendItem("markus", charprofile.Item{Name: "Кунай"})
	require.NoError(t, err)
	require.NoError(t, env.newInventoryRepo().RemoveItem("markus", "Кунай"))
	inv, _ := env.newInventoryRepo().Load("markus")
	assert.Empty(t, inv.Items)
	// Missing item is a no-op.
	require.NoError(t, env.newInventoryRepo().RemoveItem("markus", "Кунай"))
}
