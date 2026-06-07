package files

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"narrative/internal/usecase/tools"
)

// TestBuildNPCMarkdown_FullRender documents the canonical
// shape of a freshly created NPC profile. Every section the
// model filled must appear in order; empty sections are
// dropped so the file does not carry "## Темперамент\n"
// placeholders that pollute grep.
func TestBuildNPCMarkdown_FullRender(t *testing.T) {
	body := BuildNPCMarkdown(tools.NPCProfile{
		DisplayName:      "Ирука-сенсей",
		File:            "iruka",
		Nicknames:       []string{"Ирука"},
		Temperament:     "Мягкий, терпеливый, строгий по правилам.",
		Relations:       "Чуни, инструктор Академии.",
		Abilities:       "Хороший учитель, базовые навыки ниндзя.",
		PersonalMemory:  "Назначен наблюдателем за Маркусом.",
		CurrentStatus:   "Академия, ведёт урок.",
		CriticalKnowledge: "Знает правду о Наруто.",
		LastUpdate:      "День 1 — назначен наблюдателем.",
	})
	assert.Contains(t, body, "# Ирука-сенсей")
	assert.Contains(t, body, "_Прозвища: Ирука_")
	// All non-empty sections must be present, in canonical order.
	temperIdx := strings.Index(body, "## Темперамент")
	relIdx := strings.Index(body, "## Отношения с ГГ")
	abilitiesIdx := strings.Index(body, "## Способности")
	memIdx := strings.Index(body, "## Личная память/факты")
	statusIdx := strings.Index(body, "## Текущий статус")
	critIdx := strings.Index(body, "## Критические знания")
	lastIdx := strings.Index(body, "## Последнее обновление")
	assert.True(t, temperIdx >= 0)
	assert.True(t, temperIdx < relIdx, "Темперамент before Отношения")
	assert.True(t, relIdx < abilitiesIdx)
	assert.True(t, abilitiesIdx < memIdx)
	assert.True(t, memIdx < statusIdx)
	assert.True(t, statusIdx < critIdx)
	assert.True(t, critIdx < lastIdx)
	assert.Contains(t, body, "Знает правду о Наруто.")
}

// TestBuildNPCMarkdown_EmptySectionsOmitted covers the
// "create with minimal data" case: the model emits only
// the required fields and the optional ones are blank.
// The renderer must drop the empty section headers
// entirely (they're noise in the file), but must still
// render "## Отношения с другими NPC" as a placeholder
// so the model knows where to start writing on its
// first update_npc call.
func TestBuildNPCMarkdown_EmptySectionsOmitted(t *testing.T) {
	body := BuildNPCMarkdown(tools.NPCProfile{
		DisplayName:  "Хокаге",
		File:        "hokage",
		Temperament: "Мудрый, расчётливый.",
	})
	assert.Contains(t, body, "## Темперамент")
	assert.NotContains(t, body, "## Отношения с ГГ\n\n") // empty dropped
	assert.NotContains(t, body, "## Способности\n\n")
	assert.NotContains(t, body, "## Личная память/факты\n\n")
	assert.NotContains(t, body, "## Текущий статус\n\n")
	assert.NotContains(t, body, "## Критические знания\n\n")
	// The placeholder section is still there.
	assert.Contains(t, body, "## Отношения с другими NPC")
	assert.Contains(t, body, "_(допиши через update_npc)_")
}

// All tests in this file pass the SAME slug to Create and
// UpdateNPC. The default behaviour of domain.SanitizeName
// would otherwise turn "Ирука-сенсей" into "iruka_sensei"
// while "iruka" stays "iruka" — that mismatch made the
// first iteration of these tests fail with "profile not
// found" even though the file was clearly on disk.

// TestUpdateNPC_AppendsToExistingSection is the happy path
// of the operator-reported "NPC profile never grows" bug.
// After Create, a sequence of update_npc calls adds new
// facts under the right section without disturbing the
// others.
func TestUpdateNPC_AppendsToExistingSection(t *testing.T) {
	ts := newTestToolset(t)
	require.NoError(t, ts.NPC.Create("naruto", tools.NPCProfile{
		DisplayName: "Ирука-сенсей",
		File:       "iruka-sensei",
		Temperament: "Мягкий.",
		Relations:  "Чуни.",
		Abilities:  "Базовые ниндзя.",
	}))
	require.NoError(t, ts.NPC.UpdateNPC("naruto", "iruka-sensei", "Способности",
		"День 5 — продемонстрировал Хенге и Буншин перед Маркусом."))
	body, err := ts.NPC.Load("naruto", "iruka-sensei")
	require.NoError(t, err)
	assert.Contains(t, body, "Базовые ниндзя.")
	assert.Contains(t, body, "День 5 — продемонстрировал Хенге и Буншин перед Маркусом.")
	// The other section was not touched.
	assert.Contains(t, body, "Мягкий.")
	assert.Contains(t, body, "Чуни.")
}

// TestUpdateNPC_CreatesSectionIfMissing covers the case
// where the model calls update_npc with a section that
// wasn't rendered at create time (or that was rendered
// empty and stripped by the renderer). The new section
// must be inserted at the canonical position, not at the
// end of the file, so the layout stays predictable.
func TestUpdateNPC_CreatesSectionIfMissing(t *testing.T) {
	ts := newTestToolset(t)
	require.NoError(t, ts.NPC.Create("naruto", tools.NPCProfile{
		DisplayName: "Ирука-сенсей",
		File:       "iruka-sensei",
		Temperament: "Мягкий.",
	}))
	// At create time, "Критические знания" was empty and
	// was dropped from the file. Now the model reports
	// the hokage-confirmed Naruto secret. Update must
	// re-create the section at the right slot.
	require.NoError(t, ts.NPC.UpdateNPC("naruto", "iruka-sensei", "Критические знания",
		"Знает правду о Наруто."))
	body, _ := ts.NPC.Load("naruto", "iruka-sensei")
	criticalIdx := strings.Index(body, "## Критические знания")
	lastUpdateIdx := strings.Index(body, "## Последнее обновление")
	assert.True(t, criticalIdx >= 0, "section must be re-created")
	assert.True(t, lastUpdateIdx > criticalIdx, "Критические знания must come before Последнее обновление")
}

// TestUpdateNPC_LastUpdateSectionReplaces verifies the
// special contract for "## Последнее обновление":
// every update REPLACES the body, so the file always
// shows the freshest fact at the bottom. Other sections
// grow.
func TestUpdateNPC_LastUpdateSectionReplaces(t *testing.T) {
	ts := newTestToolset(t)
	require.NoError(t, ts.NPC.Create("naruto", tools.NPCProfile{
		DisplayName: "Ирука-сенсей",
		File:       "iruka-sensei",
		Temperament: "Мягкий.",
		LastUpdate:  "День 1 — назначен наблюдателем.",
	}))
	require.NoError(t, ts.NPC.UpdateNPC("naruto", "iruka-sensei", "Последнее обновление",
		"День 5 — привёл Маркуса на полигон."))
	require.NoError(t, ts.NPC.UpdateNPC("naruto", "iruka-sensei", "Последнее обновление",
		"День 7 — Хокаге посетил полигон."))
	body, _ := ts.NPC.Load("naruto", "iruka-sensei")
	assert.Contains(t, body, "День 7 — Хокаге посетил полигон.")
	assert.NotContains(t, body, "День 1 — назначен наблюдателем.")
	assert.NotContains(t, body, "День 5 — привёл Маркуса на полигон.")
}

// TestUpdateNPC_AliasesAccepted covers the model-friendly
// case where the model emits "abilities" / "relations" /
// "status" instead of the canonical Russian section
// names. The canonicaliser must accept them and route
// to the right slot.
func TestUpdateNPC_AliasesAccepted(t *testing.T) {
	ts := newTestToolset(t)
	require.NoError(t, ts.NPC.Create("naruto", tools.NPCProfile{
		DisplayName: "Ирука-сенсей",
		File:       "iruka-sensei",
		Temperament: "Мягкий.",
		Abilities:  "Стартовые.",
	}))
	require.NoError(t, ts.NPC.UpdateNPC("naruto", "iruka-sensei", "abilities",
		"День 5 — добавлен новый навык."))
	body, _ := ts.NPC.Load("naruto", "iruka-sensei")
	assert.Contains(t, body, "День 5 — добавлен новый навык.")
}

// TestUpdateNPC_RejectsUnknownSection covers the boundary
// where the model emits a section name that does not match
// any canonical or alias. The update must fail with a
// descriptive error rather than silently dropping the
// fact or creating a junk section.
func TestUpdateNPC_RejectsUnknownSection(t *testing.T) {
	ts := newTestToolset(t)
	require.NoError(t, ts.NPC.Create("naruto", tools.NPCProfile{
		DisplayName: "Ирука-сенсей",
		File:       "iruka-sensei",
		Temperament: "Мягкий.",
	}))
	err := ts.NPC.UpdateNPC("naruto", "iruka-sensei", "favorite_colour", "синий")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown section")
}

// TestUpdateNPC_NotFoundForUnknownNPC covers the
// "model called update_npc before create_npc" path.
// The error must be the typed ErrNPCNotFound so the
// dispatcher can surface a specific corrective message
// ("create the NPC first") rather than a generic IO error.
func TestUpdateNPC_NotFoundForUnknownNPC(t *testing.T) {
	ts := newTestToolset(t)
	err := ts.NPC.UpdateNPC("naruto", "ghost", "Способности", "факт")
	require.ErrorIs(t, err, ErrNPCNotFound)
}

// TestLoad_NotFoundForEmptyFile documents that Load
// returns ErrNPCNotFound for both "file missing" and
// "file present but blank" — the GM uses Load to detect
// "this NPC has no profile yet" and must not be confused
// by a zero-byte placeholder.
func TestLoad_NotFoundForEmptyFile(t *testing.T) {
	ts := newTestToolset(t)
	require.NoError(t, ts.NPC.fs.WriteRawAtomic("worlds/naruto/characters/blank.md", ""))
	_, err := ts.NPC.Load("naruto", "blank")
	assert.ErrorIs(t, err, ErrNPCNotFound)
}
