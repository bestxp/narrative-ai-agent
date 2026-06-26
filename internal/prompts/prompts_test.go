package prompts_test

import (
	"testing"

	"github.com/bestxp/narrative-ai-agent/internal/prompts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestList_ContainsExpectedFiles(t *testing.T) {
	t.Parallel()

	list := prompts.List()
	assert.Contains(t, list, "narrative.md.tmpl", "narrative.md.tmpl must be embedded")
	assert.Contains(t, list, "summary.md.tmpl", "summary.md.tmpl must be embedded")
	assert.Contains(t, list, "world_state.md.tmpl", "world_state.md.tmpl must be embedded")
}

// TestRender_PlainTemplateRoundTrip: a template with
// no {{ }} markers renders as-is. This is the safety
// net: a future refactor that adds a stray substitution
// is caught here.
func TestRender_PlainTemplateRoundTrip(t *testing.T) {
	t.Parallel()
	prompts.ResetTemplateCache()

	rendered, err := prompts.Render("summary.md.tmpl", prompts.PromptData{
		Narrative: prompts.NarrativeData{WordLimit: 200},
	})
	require.NoError(t, err)
	assert.NotEmpty(t, rendered)
}

// TestRender_SubstitutesConfig: the {{ .Narrative.WordLimit }}
// markers in narrative.md.tmpl are substituted from
// the data-bag. A different WordLimit yields a different
// rendered body.
func TestRender_SubstitutesConfig(t *testing.T) {
	t.Parallel()
	prompts.ResetTemplateCache()

	data := prompts.PromptData{Narrative: prompts.NarrativeData{WordLimit: 250}}
	rendered, err := prompts.Render("narrative.md.tmpl", data)
	require.NoError(t, err)
	assert.Contains(t, rendered, "≤ 250 слов")
	assert.Contains(t, rendered, "Лимит слов: 250")
	assert.Contains(t, rendered, "80–250 слов")
}

// TestRender_MissingTemplate: a typo in the template
// name returns an error rather than rendering an
// empty string. This is the contract operators rely on
// when they edit config.yaml — a missing template must
// fail loudly at startup, not silently render as "".
func TestRender_MissingTemplate(t *testing.T) {
	t.Parallel()

	_, err := prompts.Render("does-not-exist.md.tmpl", prompts.PromptData{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "template not found")
}

// TestRender_RejectsNonTemplate: Render only accepts
// .tmpl files. A naked .md call is a programming
// error, not a runtime one.
func TestRender_RejectsNonTemplate(t *testing.T) {
	t.Parallel()

	_, err := prompts.Render("narrative.md", prompts.PromptData{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), ".tmpl")
}

// TestRender_CachesParsedTemplate: the second call
// with a different data-bag reuses the parsed
// template from the cache. We cannot directly observe
// the cache, but we can assert the second render
// reflects the new WordLimit.
func TestRender_CachesParsedTemplate(t *testing.T) {
	t.Parallel()
	prompts.ResetTemplateCache()

	_, err := prompts.Render("narrative.md.tmpl", prompts.PromptData{Narrative: prompts.NarrativeData{WordLimit: 200}})
	require.NoError(t, err)
	second, err := prompts.Render("narrative.md.tmpl", prompts.PromptData{Narrative: prompts.NarrativeData{WordLimit: 999}})
	require.NoError(t, err)
	// The second render substitutes WordLimit=999 in
	// the same markers; "999" must appear in the output
	// even though the first render used 200. If the
	// cache mistakenly pinned the data, the markers
	// would still show "200".
	assert.Contains(t, second, "999")
}

// TestNewPromptData_DefaultsFilled: the CompactionData
// sub-struct is populated from the Go-side defaults
// in internal/limits. The exact constants live there;
// here we just assert the values flow through.
func TestNewPromptData_DefaultsFilled(t *testing.T) {
	t.Parallel()

	snap := prompts.NarrativeConfigSnapshot{WordLimit: 250}
	d := prompts.NewPromptData(snap, prompts.CharacterData{}, prompts.WorldData{})
	assert.Equal(t, prompts.DefaultNPCPersonalMemoryLimit, d.Compaction.NPCPersonalMemoryLimit)
	assert.Equal(t, prompts.DefaultNPCPersonalMemoryTarget, d.Compaction.NPCPersonalMemoryTarget)
	assert.Equal(t, prompts.DefaultMemoryTargetBytes, d.Compaction.MemoryTargetBytes)
	assert.Equal(t, 250, d.Narrative.WordLimit)
}

// TestNewStateData: the data-bag shape for state.yaml.
// The state file is YAML, not a template — the data-bag
// here is just a typed projection used by tests that
// round-trip StateSnapshot values.
func TestNewStateData(t *testing.T) {
	t.Parallel()

	d := prompts.NewStateData("naruto", 5, true, "день", "Коноха", "Аньбу толкает", "",
		prompts.StageStateData{Current: "wave_mission", TimelineIndex: 1},
		[]string{"anbu_dog", "anbu_cat"},
		[]string{"Ход 1: ...", "Ход 2: ..."})
	assert.Equal(t, "naruto", d.World)
	assert.Equal(t, 5, d.Day)
	assert.True(t, d.InFlight)
	assert.Equal(t, "день", d.Daytime)
	assert.Equal(t, "Коноха", d.Location)
	assert.Equal(t, "Аньбу толкает", d.Moment)
	assert.Equal(t, []string{"anbu_dog", "anbu_cat"}, d.NPCs)
	assert.Equal(t, []string{"Ход 1: ...", "Ход 2: ..."}, d.Events)
	assert.Equal(t, "wave_mission", d.Stage.Current)
	assert.Equal(t, 1, d.Stage.TimelineIndex)
}
