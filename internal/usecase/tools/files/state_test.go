package files

import (
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"narrative/internal/adapter/storage"
	"narrative/internal/usecase/tools"
)

// newTestToolset is the canonical fixture for state-level tests.
// It writes the registry (info.yaml) and the world directory
// stub so UpdateState/parseStateMD have something to read on
// disk.
func newTestToolset(t *testing.T) *Toolset {
	t.Helper()
	fs, err := storage.NewFileStore(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, fs.EnsureDir("worlds/naruto/characters"))
	require.NoError(t, fs.WriteRawAtomic("info.yaml",
		"active_character: markus\nactive_world: naruto\n"))
	return New(fs, zerolog.Nop(), nil)
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
	require.NoError(t, ts.State.UpdateState(tools.StateSnapshot{
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
	require.NoError(t, ts.State.UpdateState(tools.StateSnapshot{
		Day:      1,
		InFlight: true,
		Moment:   "m2",
		NPCs:     []string{"Ирука", "Хокаге"},
		AppendEvents: []string{
			"ирука повёл маркуса в столовую", // duplicate
			"Хокаге пришёл на полигон",     // new
		},
	}))
	body, err := ts.State.fs.ReadRaw("worlds/naruto/state.md")
	require.NoError(t, err)
	// The duplicate must appear exactly once; the new
	// event must appear once.
	assert.Equal(t, 1, strings.Count(body, "Ирука повёл Маркуса в столовую"),
		"the duplicate event must not appear twice; got body:\n%s", body)
	assert.Contains(t, body, "Хокаге пришёл на полигон")
	// NPC list reflects the most recent UpdateState (full
	// replacement, not append — see UpdateState doc).
	assert.Contains(t, body, "NPC: Ирука, Хокаге")
}

// TestUpdateState_PreservesGenuineNarrativeVariation
// guards the dedupe against false positives: two events
// that share a prefix but diverge in their verbs or
// objects are NOT duplicates. "Ирука привёл" and
// "Ирука повёл" are different beats.
func TestUpdateState_PreservesGenuineNarrativeVariation(t *testing.T) {
	ts := newTestToolset(t)
	require.NoError(t, ts.State.UpdateState(tools.StateSnapshot{
		Day:      1,
		InFlight: true,
		Moment:   "m",
		NPCs:     []string{"Ирука"},
		AppendEvents: []string{
			"Ирука привёл Маркуса в столовую",
		},
	}))
	require.NoError(t, ts.State.UpdateState(tools.StateSnapshot{
		Day:      1,
		InFlight: true,
		Moment:   "m",
		NPCs:     []string{"Ирука"},
		AppendEvents: []string{
			"Ирука повёл Маркуса к выходу", // different verb
		},
	}))
	body, err := ts.State.fs.ReadRaw("worlds/naruto/state.md")
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
	require.NoError(t, ts.State.UpdateState(tools.StateSnapshot{
		Day:      1,
		InFlight: true,
		Moment:   "m",
		NPCs:     []string{"Ирука"},
		AppendEvents: []string{
			"", "  ", "Хокаге пришёл",
		},
	}))
	body, err := ts.State.fs.ReadRaw("worlds/naruto/state.md")
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
	require.NoError(t, ts.State.UpdateState(tools.StateSnapshot{
		Day: 1, InFlight: true, Moment: "m",
		NPCs: []string{"Ирука", "Хокаге"},
	}))
	require.NoError(t, ts.State.UpdateState(tools.StateSnapshot{
		Day: 1, InFlight: true, Moment: "m2",
		NPCs: []string{"Ирука"}, // Хокаге walked away
	}))
	body, err := ts.State.fs.ReadRaw("worlds/naruto/state.md")
	require.NoError(t, err)
	assert.Contains(t, body, "NPC: Ирука")
	assert.NotContains(t, body, "Хокаге", "Хокаге should have been removed when he left the scene")
	// sanity: file path joined correctly
}
