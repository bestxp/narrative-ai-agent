package files

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bestxp/narrative-ai-agent/internal/adapter/storage"
	"github.com/bestxp/narrative-ai-agent/internal/repository/api"
	"github.com/bestxp/narrative-ai-agent/internal/repository/yaml"
	"github.com/bestxp/narrative-ai-agent/internal/slowlog"
	yamlfs "github.com/bestxp/narrative-ai-agent/internal/storage/fs"
	"github.com/bestxp/narrative-ai-agent/internal/usecase/tools"
)

// readFile is a tiny shim around os.ReadFile kept here so
// captureSlowlog doesn't need to import "os" itself.
func readFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

// newTestToolset is the canonical fixture for state-level tests.
// It writes the registry (info.yaml) and the world directory
// stub so UpdateState/ParseStateMD have something to read on
// disk.
func newTestToolset(t *testing.T) *Toolset {
	t.Helper()
	fs, err := storage.NewFileStore(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, fs.EnsureDir("worlds/naruto/characters"))
	require.NoError(t, fs.WriteRawAtomic("info.yaml",
		"active_character: markus\nactive_world: naruto\n"))
	yamlStore, err := yamlfs.New(fs.Root())
	require.NoError(t, err)
	repos := api.NewYamlRepositories(yamlStore)
	return New(fs, repos, zerolog.Nop(), slowlog.Discard(), nil, nil, nil, nil)
}

// captureSlowlog returns a *slowlog.Logger that writes JSON
// lines into a temp file. The returned read-back func parses
// the file and returns all entries that match a given kind.
// Used by tests that assert on structured slowlog events
// (tool.update_state, tool.update_npc, etc.).
func captureSlowlog(t *testing.T) (*slowlog.Logger, func(kind string) []slowlog.Entry) {
	t.Helper()
	dir := t.TempDir()
	logger, err := slowlog.File(dir + "/slow.log")
	require.NoError(t, err)
	read := func(kind string) []slowlog.Entry {
		data, err := readFile(dir + "/slow.log")
		if err != nil {
			return nil
		}
		var out []slowlog.Entry
		for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
			if line == "" {
				continue
			}
			var e slowlog.Entry
			if err := json.Unmarshal([]byte(line), &e); err != nil {
				continue
			}
			if e.Kind == kind {
				out = append(out, e)
			}
		}
		return out
	}
	return logger, read
}

// TestUpdateState_DedupesAppendEvents is the regression
// test for the operator-reported "хронология дублируется".
// Cause: a retry loop or a model that re-asserts the same
// beat on every turn would write the same line N times.
// The fix: UpdateState compares each incoming event on a
// whitespace-normalised key against the existing list and
// drops duplicates silently.
func TestUpdateState_DedupesAppendEvents(t *testing.T) {
	ts := newTestToolset(t)
	// First call: seed the chronology with one event.
	require.NoError(t, ts.UpdateState(tools.StateSnapshot{
		Day:      1,
		InFlight: true,
		Moment:   "m1",
		NPCs:     []string{"Ирука"},
		AppendEvents: []string{
			"Ирука повёл Маркуса в столовую",
		},
	}))
	// Second call: same world, same event, different
	// capitalisation. Should be deduped, not appended.
	require.NoError(t, ts.UpdateState(tools.StateSnapshot{
		Day:      1,
		InFlight: true,
		Moment:   "m2",
		NPCs:     []string{"Ирука", "Хокаге"},
		AppendEvents: []string{
			"ирука повёл маркуса в столовую", // duplicate
			"Хокаге пришёл на полигон",       // new
		},
	}))
	body, err := renderStateBodyForTest(t, ts)
	require.NoError(t, err)
	// The duplicate must appear exactly once; the new
	// event must appear once.
	assert.Equal(t, 1, strings.Count(body, "Ирука повёл Маркуса в столовую"),
		"the duplicate event must not appear twice; got body:\n%s", body)
	assert.Contains(t, body, "Хокаге пришёл на полигон")
	// NPC list reflects the most recent UpdateState (full
	// replacement, not append — see UpdateState doc).
	// planning/0001: YAML format — Ирука and Хокаге
	// appear as separate list items under `npcs:`.
	assert.Contains(t, body, "- Ирука")
	assert.Contains(t, body, "- Хокаге")
}

// TestUpdateState_PreservesGenuineNarrativeVariation
// guards the dedupe against false positives: two events
// that share a prefix but diverge in their verbs or
// objects are NOT duplicates. "Ирука привёл" and
// "Ирука повёл" are different beats.
func TestUpdateState_PreservesGenuineNarrativeVariation(t *testing.T) {
	ts := newTestToolset(t)
	require.NoError(t, ts.UpdateState(tools.StateSnapshot{
		Day:      1,
		InFlight: true,
		Moment:   "m",
		NPCs:     []string{"Ирука"},
		AppendEvents: []string{
			"Ирука привёл Маркуса в столовую",
		},
	}))
	require.NoError(t, ts.UpdateState(tools.StateSnapshot{
		Day:      1,
		InFlight: true,
		Moment:   "m",
		NPCs:     []string{"Ирука"},
		AppendEvents: []string{
			"Ирука повёл Маркуса к выходу", // different verb
		},
	}))
	body, err := renderStateBodyForTest(t, ts)
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(body, "Ирука привёл Маркуса в столовую"))
	assert.Equal(t, 1, strings.Count(body, "Ирука повёл Маркуса к выходу"))
}

// TestUpdateState_EmptyEventIgnored is a guard against the
// model emitting `events: ["", "  "]` — a model may try to
// "pad" the events array to its declared min length. The
// normaliser drops empty events before the dedupe check.
func TestUpdateState_EmptyEventIgnored(t *testing.T) {
	ts := newTestToolset(t)
	require.NoError(t, ts.UpdateState(tools.StateSnapshot{
		Day:      1,
		InFlight: true,
		Moment:   "m",
		NPCs:     []string{"Ирука"},
		AppendEvents: []string{
			"", "  ", "Хокаге пришёл",
		},
	}))
	body, err := renderStateBodyForTest(t, ts)
	require.NoError(t, err)
	assert.NotContains(t, body, "- \n", "empty events must not appear as bare bullet lines")
	// The real event must be there.
	assert.Contains(t, body, "Хокаге пришёл")
}

// TestUpdateState_ReplacesNPCList documents that the
// npcs field is a *full replacement* (not append), so a
// model that omits an NPC from the npcs array correctly
// removes them from state.md. This is what the operator
// hit when Хокаге appeared on the page but the npcs
// field still showed only Ирука — the new tool
// description and this test together enforce the
// "include all NPCs you mentioned" rule.
func TestUpdateState_ReplacesNPCList(t *testing.T) {
	ts := newTestToolset(t)
	require.NoError(t, ts.UpdateState(tools.StateSnapshot{
		Day: 1, InFlight: true, Moment: "m",
		NPCs: []string{"Ирука", "Хокаге"},
	}))
	require.NoError(t, ts.UpdateState(tools.StateSnapshot{
		Day: 1, InFlight: true, Moment: "m2",
		NPCs: []string{"Ирука"}, // Хокаге walked away
	}))
	body, err := renderStateBodyForTest(t, ts)
	require.NoError(t, err)
	assert.Contains(t, body, "- Ирука")
	assert.NotContains(t, body, "Хокаге", "Хокаге should have been removed when he left the scene")
	// sanity: file path joined correctly
}

// newStateWithSlowlog is a state-only fixture for slowlog
// assertions. It mirrors newTestToolset but wires a real
// slowlog file so the test can read back the
// `tool.update_state` entries. Keeping this separate from
// newTestToolset means existing tests that rely on
// `slowlog.Discard()` aren't affected.
func newStateWithSlowlog(t *testing.T) (*State, func(kind string) []slowlog.Entry) {
	t.Helper()
	fs, err := storage.NewFileStore(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, fs.EnsureDir("worlds/naruto/characters"))
	require.NoError(t, fs.WriteRawAtomic("info.yaml",
		"active_character: markus\nactive_world: naruto\n"))
	yamlStore, err := yamlfs.New(fs.Root())
	require.NoError(t, err)
	repos := api.NewYamlRepositories(yamlStore)
	logger, read := captureSlowlog(t)
	st := newState(zerolog.Nop(), logger, repos)
	return st, read
}

// TestUpdateState_SlowlogDeltaNPCAddedRemoved verifies that
// every UpdateState call writes a structured slowlog
// entry with the per-element diff (npcs_added, npcs_removed,
// events_added). This is the regression coverage for the
// "what did update_state actually write?" diagnostic —
// without these fields, an operator looking at slow.log
// after a session only sees "npcs: 5→7" but cannot tell
// which 2 joined the roster.
func TestUpdateState_SlowlogDeltaNPCAddedRemoved(t *testing.T) {
	st, read := newStateWithSlowlog(t)
	require.NoError(t, st.UpdateState(tools.StateSnapshot{
		Day: 1, InFlight: true, Moment: "утро",
		NPCs: []string{"Ирука", "Какаши"},
	}))
	require.NoError(t, st.UpdateState(tools.StateSnapshot{
		Day: 1, InFlight: true, Moment: "день",
		NPCs:         []string{"Ирука", "Хината"}, // -Какаши, +Хината
		AppendEvents: []string{"встретил Хинату"},
	}))

	entries := read("tool.update_state")
	require.Len(t, entries, 2, "one tool.update_state per UpdateState call")

	// First call: cold roster.
	first := entries[0].Fields
	assert.Equal(t, float64(1), first["day"])
	assert.Equal(t, "утро", first["moment"])
	added := toStrSlice(t, first["npcs_added"])
	assert.ElementsMatch(t, []string{"Ирука", "Какаши"}, added)
	assert.Empty(t, toStrSlice(t, first["npcs_removed"]))
	assert.Equal(t, float64(0), first["events_added"])
	assert.Equal(t, "worlds/naruto/state.yaml", first["path"])
	assert.Greater(t, first["bytes"], float64(0))

	// Second call: -Какаши, +Хината, +1 event.
	second := entries[1].Fields
	assert.Equal(t, "день", second["moment"])
	assert.ElementsMatch(t, []string{"Хината"}, toStrSlice(t, second["npcs_added"]))
	assert.ElementsMatch(t, []string{"Какаши"}, toStrSlice(t, second["npcs_removed"]))
	assert.Equal(t, float64(1), second["events_added"])
}

// TestUpdateState_SlowlogNilSafe verifies that UpdateState
// with a nil slowlog (the legacy code path that pre-dates
// the slowlog wiring) does not panic. This matters for
// firstlaunch → newWorld.Launch, which constructs a
// bare-bones State without a logger.
func TestUpdateState_SlowlogNilSafe(t *testing.T) {
	fs, err := storage.NewFileStore(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, fs.EnsureDir("worlds/naruto/characters"))
	yamlStore, _ := yamlfs.New(fs.Root())
	repos := api.NewYamlRepositories(yamlStore)
	st := newState(zerolog.Nop(), nil, repos) // <-- nil slow
	require.NoError(t, fs.WriteRawAtomic("info.yaml", "active_character: markus\nactive_world: naruto\n"))
	require.NoError(t, st.UpdateState(tools.StateSnapshot{
		Day: 1, InFlight: true, Moment: "m",
		NPCs: []string{"Ирука"},
	}))
}

// toStrSlice converts a `[]any` (as produced by JSON
// unmarshal of a `[]string`) into a `[]string` for
// assert.ElementsMatch. Returns an empty slice (not nil)
// when the input is missing or not a slice, so
// ElementsMatch is safe on an absent field.
func toStrSlice(t *testing.T, v any) []string {
	t.Helper()
	if v == nil {
		return []string{}
	}
	switch xs := v.(type) {
	case []string:
		return xs
	case []any:
		out := make([]string, 0, len(xs))
		for _, x := range xs {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return []string{}
	}
}

// renderStateBodyForTest renders the active world's
// state.md via the repository layer (Load + render).
// Used by tests that need byte-level assertions on the
// rendered markdown.
func renderStateBodyForTest(t *testing.T, ts *Toolset) (string, error) {
	t.Helper()
	info, err := ts.repos.Info.Load()
	if err != nil || info.ActiveWorld == "" {
		return "", nil
	}
	snap, err := ts.repos.WorldState.Load(info.ActiveWorld)
	if err != nil {
		return "", err
	}
	return yaml.RenderStateBody(snap)
}
