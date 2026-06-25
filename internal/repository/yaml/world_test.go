package yaml

import (
	"strings"
	"testing"

	"github.com/bestxp/narrative-ai-agent/internal/chronicle"
	"github.com/bestxp/narrative-ai-agent/internal/domain"
	"github.com/bestxp/narrative-ai-agent/internal/storage"
	"github.com/bestxp/narrative-ai-agent/internal/storage/fs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testEnv bundles the storage backend. Each *_test.go
// file in this package adds the per-domain
// constructors (SoulRepo, NPCRepo, ...) it needs as
// methods on this type.
type testEnv struct {
	store storage.Storage
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	s, err := fs.New(t.TempDir())
	if err != nil {
		t.Fatalf("fs.New: %v", err)
	}
	return &testEnv{store: s}
}

func (e *testEnv) newInfoRepo() *InfoYaml             { return NewInfoYaml(e.store) }
func (e *testEnv) newWorldStateRepo() *WorldStateYaml { return NewWorldStateYaml(e.store) }
func (e *testEnv) newPlanRepo() *PlanYaml             { return NewPlanYaml(e.store) }
func (e *testEnv) newLoreRepo() *LoreYaml             { return NewLoreYaml(e.store) }
func (e *testEnv) newCanonRepo() *CanonYaml           { return NewCanonYaml(e.store) }
func (e *testEnv) newChronicleRepo() *ChronicleYaml   { return NewChronicleYaml(e.store) }

// fs returns the underlying storage. Used by tests
// that simulate operator hand-edits of files the bot
// never writes (canon.md).
func (e *testEnv) fs() storage.Storage { return e.store }

func TestInfoYaml_RoundTrip(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	r := env.newInfoRepo()
	in := domain.Info{
		ActiveCharacter: "markus",
		ActiveWorld:     "naruto",
		Characters:      []string{"markus", "alice"},
		Worlds:          []string{"naruto", "bleach"},
	}
	require.NoError(t, r.Save(in))

	out, err := r.Load()
	require.NoError(t, err)
	assert.Equal(t, in.ActiveCharacter, out.ActiveCharacter)
	assert.Equal(t, in.ActiveWorld, out.ActiveWorld)
	assert.ElementsMatch(t, in.Characters, out.Characters)
	assert.ElementsMatch(t, in.Worlds, out.Worlds)
}

func TestInfoYaml_LoadMissing(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	out, err := env.newInfoRepo().Load()
	require.NoError(t, err)
	assert.Equal(t, domain.Info{}, out)
}

func TestWorldStateYaml_RoundTrip(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	r := env.newWorldStateRepo()
	in := domain.StateSnapshot{
		World:    "naruto",
		Day:      5,
		InFlight: true,
		Location: "Академия",
		Moment:   "Утро, тренировка",
		NPCs:     []string{"Какаши", "Ирука"},
		Events:   []string{"Наруто пришёл", "Столовая"},
	}
	require.NoError(t, r.Save("naruto", in))

	out, err := r.Load("naruto")
	require.NoError(t, err)
	assert.Equal(t, in, out)
}

func TestWorldStateYaml_AppendEvent(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	r := env.newWorldStateRepo()
	require.NoError(t, r.Save("naruto", domain.StateSnapshot{
		World:  "naruto",
		Day:    1,
		Events: []string{"first"},
	}))
	require.NoError(t, r.AppendEvent("naruto", "second"))
	require.NoError(t, r.AppendEvent("naruto", "first"))
	require.NoError(t, r.AppendEvent("naruto", "  first  "))
	out, err := r.Load("naruto")
	require.NoError(t, err)
	assert.Equal(t, []string{"first", "second"}, out.Events)
}

func TestWorldStateYaml_EnsureExists(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	r := env.newWorldStateRepo()
	require.NoError(t, r.EnsureExists("naruto", 1, true))
	out, err := r.Load("naruto")
	require.NoError(t, err)
	assert.Equal(t, "naruto", out.World)
	assert.Equal(t, 1, out.Day)
	assert.True(t, out.InFlight)

	require.NoError(t, r.EnsureExists("naruto", 99, false))
	out, _ = r.Load("naruto")
	assert.Equal(t, 1, out.Day, "EnsureExists must not overwrite")
}

func TestPlanYaml_RoundTrip(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	r := env.newPlanRepo()
	body := "## План\n- event 1\n- event 2\n- event 3\n"
	require.NoError(t, r.Save("naruto", body))
	out, err := r.Load("naruto")
	require.NoError(t, err)
	assert.Equal(t, body, out)
}

func TestPlanYaml_ReplaceEvents(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	r := env.newPlanRepo()
	require.NoError(t, r.ReplaceEvents(t.Context(), "naruto", []string{"a", "b", "c"}))
	out, err := r.Load("naruto")
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b", "c"}, ParsePlanEvents(out))
}

func TestPlanYaml_ReplaceEvents_OutOfRange(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	r := env.newPlanRepo()
	err := r.ReplaceEvents(t.Context(), "naruto", []string{"a", "b"})
	require.Error(t, err)
	err = r.ReplaceEvents(t.Context(), "naruto", []string{"a", "b", "c", "d", "e", "f"})
	require.Error(t, err)
}

func TestLoreYaml_RoundTrip(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	r := env.newLoreRepo()
	body := "## Header 1\n- bullet\n\n## Header 2\n- bullet 2\n"
	require.NoError(t, r.Save("naruto", body))
	out, err := r.Load("naruto")
	require.NoError(t, err)
	assert.Equal(t, body, out)
}

func TestLoreYaml_AppendEntry(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	r := env.newLoreRepo()
	require.NoError(t, r.AppendEntry("naruto", "Header 1", "bullet 1"))
	require.NoError(t, r.AppendEntry("naruto", "Header 2", "bullet 2"))
	out, err := r.Load("naruto")
	require.NoError(t, err)
	assert.Contains(t, out, "## Header 1")
	assert.Contains(t, out, "- bullet 1")
	assert.Contains(t, out, "## Header 2")
	assert.Contains(t, out, "- bullet 2")
}

func TestLoreYaml_AppendEntry_EmptyRejected(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	r := env.newLoreRepo()
	err := r.AppendEntry("naruto", "", "x")
	require.Error(t, err)
	err = r.AppendEntry("naruto", "x", "")
	require.Error(t, err)
}

func TestCanonYaml_Load(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	r := env.newCanonRepo()
	out, err := r.Load("naruto")
	require.NoError(t, err)
	assert.Empty(t, out)
	body, _ := r.Load("naruto")
	assert.Empty(t, body)

	require.NoError(t, env.fs().Write("worlds/naruto/canon.md", []byte("# canon\n")))
	out, _ = r.Load("naruto")
	assert.True(t, strings.HasPrefix(out, "# canon"))
	body, _ = r.Load("naruto")
	assert.True(t, strings.HasPrefix(body, "# canon"))
}

func TestChronicleYaml_RoundTrip(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	r := env.newChronicleRepo()
	in := chronicle.Chronicle{
		Periods: []chronicle.Period{
			{From: 1, To: 30, Memory: "first window"},
		},
		Days: map[int]string{5: "raw day 5"},
	}
	require.NoError(t, r.Save("naruto", in))

	out, err := r.Load("naruto")
	require.NoError(t, err)
	assert.Len(t, out.Periods, len(in.Periods))
	assert.Equal(t, in.Periods[0], out.Periods[0])
	assert.Equal(t, "raw day 5", out.Days[5])
}

func TestChronicleYaml_LoadMissingReturnsEmpty(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	r := env.newChronicleRepo()
	out, err := r.Load("naruto")
	require.NoError(t, err)
	assert.Empty(t, out.Periods)
	assert.Empty(t, out.Days)
}
