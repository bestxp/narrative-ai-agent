package chronicle_test

import (
	"testing"

	"github.com/bestxp/narrative-ai-agent/internal/chronicle"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoad_Empty(t *testing.T) {
	t.Parallel()

	_, err := chronicle.Load("")
	require.ErrorIs(t, err, chronicle.ErrNotFound)
}

func TestLoad_WhitespaceOnly(t *testing.T) {
	t.Parallel()

	_, err := chronicle.Load("   \n\n  \n")
	require.ErrorIs(t, err, chronicle.ErrNotFound)
}

func TestSaveLoad_RoundTrip(t *testing.T) {
	t.Parallel()

	in := chronicle.Chronicle{
		Periods: []chronicle.Period{
			{From: 1, To: 30, Memory: "first window summary"},
			{From: 31, To: 60, Memory: "second window"},
		},
		Days: map[int]string{
			61: "raw day 61",
			62: "raw day 62",
		},
	}

	body, err := in.Save()
	require.NoError(t, err, "Save")

	out, err := chronicle.Load(body)
	require.NoError(t, err, "chronicle.Load")

	require.Len(t, out.Periods, 2)
	assert.Equal(t, chronicle.Period{From: 1, To: 30, Memory: "first window summary"}, out.Periods[0])
	assert.Equal(t, chronicle.Period{From: 31, To: 60, Memory: "second window"}, out.Periods[1])
	require.Len(t, out.Days, 2)
	assert.Equal(t, "raw day 61", out.Days[61])
	assert.Equal(t, "raw day 62", out.Days[62])
}

func TestLoad_OnlyPeriods(t *testing.T) {
	t.Parallel()

	body := `periods:
  - from: 1
    to: 30
    memory: "summary"
`

	c, err := chronicle.Load(body)
	require.NoError(t, err, "chronicle.Load")

	require.Len(t, c.Periods, 1)
	assert.Empty(t, c.Days)
}

func TestLoad_OnlyDays(t *testing.T) {
	t.Parallel()

	body := `days:
  1: "raw"
  2: "raw 2"
`

	c, err := chronicle.Load(body)
	require.NoError(t, err, "chronicle.Load")

	assert.Empty(t, c.Periods)
	require.Len(t, c.Days, 2)
}

func TestAppendDay_Basic(t *testing.T) {
	t.Parallel()

	c := chronicle.Chronicle{}
	require.True(t, c.AppendDay(1, "day 1"), "AppendDay should return true on first insert")
	require.True(t, c.AppendDay(2, "day 2"), "AppendDay should return true for new day")
	require.False(t, c.AppendDay(1, "duplicate"), "AppendDay should return false for duplicate day")
	assert.Equal(t, "day 1", c.Days[1])
}

func TestAppendDay_Trims(t *testing.T) {
	t.Parallel()

	c := chronicle.Chronicle{}
	c.AppendDay(1, "  hello  ")

	assert.Equal(t, "hello", c.Days[1], "AppendDay must trim surrounding whitespace")
}

func TestAppendDay_EmptyText(t *testing.T) {
	t.Parallel()

	c := chronicle.Chronicle{}
	require.False(t, c.AppendDay(1, ""), "AppendDay should return false for empty text")
	require.False(t, c.AppendDay(1, "   "), "AppendDay should return false for whitespace-only text")
}

func TestAppendDay_ZeroDay(t *testing.T) {
	t.Parallel()

	c := chronicle.Chronicle{}
	require.False(t, c.AppendDay(0, "x"), "AppendDay should return false for day=0")
	require.False(t, c.AppendDay(-1, "x"), "AppendDay should return false for negative day")
}

func TestCompressWindow_Basic(t *testing.T) {
	t.Parallel()

	c := chronicle.Chronicle{Days: map[int]string{
		1: "a", 2: "b", 3: "c",
	}}
	require.NoError(t, c.CompressWindow(1, 3, "compressed"))

	require.Len(t, c.Periods, 1)
	assert.Equal(t, chronicle.Period{From: 1, To: 3, Memory: "compressed"}, c.Periods[0])
	assert.Empty(t, c.Days)
}

func TestCompressWindow_PreservesOutOfRangeDays(t *testing.T) {
	t.Parallel()

	c := chronicle.Chronicle{Days: map[int]string{
		1: "a", 2: "b", 3: "c", 4: "d", 5: "e",
	}}
	require.NoError(t, c.CompressWindow(2, 4, "mid"))

	require.Len(t, c.Days, 2)

	_, ok1 := c.Days[1]
	_, ok5 := c.Days[5]

	assert.True(t, ok1, "day 1 should survive")
	assert.True(t, ok5, "day 5 should survive")
}

func TestCompressWindow_RejectsOverlap(t *testing.T) {
	t.Parallel()

	c := chronicle.Chronicle{Days: map[int]string{
		31: "x", 32: "y",
	}}
	// Pretend a period for days 1..30 was already
	// written by a previous call.
	c.Periods = append(c.Periods, chronicle.Period{From: 1, To: 30, Memory: "old"})

	err := c.CompressWindow(20, 40, "should fail")
	require.Error(t, err, "expected error on overlap")
	assert.Contains(t, err.Error(), "inside closed period")
	// chronicle.Chronicle should be left untouched.
	assert.Len(t, c.Periods, 1, "periods list should not change on error")
	assert.Len(t, c.Days, 2, "days list should not change on error")
}

func TestCompressWindow_InvalidRange(t *testing.T) {
	t.Parallel()

	c := chronicle.Chronicle{}
	require.Error(t, c.CompressWindow(10, 5, "x"), "expected error when from > to")
	require.Error(t, c.CompressWindow(0, 5, "x"), "expected error when from = 0")
}

func TestCompressWindow_EmptyMemory(t *testing.T) {
	t.Parallel()

	c := chronicle.Chronicle{}
	require.Error(t, c.CompressWindow(1, 5, ""), "expected error on empty memory")
	require.Error(t, c.CompressWindow(1, 5, "   "), "expected error on whitespace-only memory")
}

func TestLastDay(t *testing.T) {
	t.Parallel()

	c := chronicle.Chronicle{}
	_, ok := c.LastDay()
	require.False(t, ok, "LastDay should be false on empty chronicle")

	c.Days = map[int]string{1: "a", 5: "b", 3: "c"}

	got, ok := c.LastDay()
	require.True(t, ok)
	assert.Equal(t, 5, got)
}

func TestLastPeriodEnd(t *testing.T) {
	t.Parallel()

	c := chronicle.Chronicle{}
	_, ok := c.LastPeriodEnd()
	require.False(t, ok, "LastPeriodEnd should be false on empty chronicle")

	c.Periods = []chronicle.Period{
		{From: 1, To: 30, Memory: "a"},
		{From: 31, To: 45, Memory: "b"},
		{From: 46, To: 60, Memory: "c"},
	}

	got, ok := c.LastPeriodEnd()
	require.True(t, ok)
	assert.Equal(t, 60, got)
}

func TestSortedDays(t *testing.T) {
	t.Parallel()

	c := chronicle.Chronicle{Days: map[int]string{
		3: "c", 1: "a", 2: "b",
	}}
	out := c.SortedDays()
	want := []int{1, 2, 3}

	require.Len(t, out, len(want))

	for i, n := range want {
		assert.Equal(t, n, out[i].Number, "entry %d", i)
	}
}

func TestSave_EmptyChronicleHasBothSections(t *testing.T) {
	t.Parallel()

	c := chronicle.Chronicle{}

	body, err := c.Save()
	require.NoError(t, err, "Save")

	assert.Contains(t, body, "periods:")
	assert.Contains(t, body, "days:")
}

func TestSaveLoad_EmptyChronicle(t *testing.T) {
	t.Parallel()

	c := chronicle.Chronicle{}

	body, err := c.Save()
	require.NoError(t, err, "Save")

	out, err := chronicle.Load(body)
	require.NoError(t, err, "chronicle.Load")

	assert.Empty(t, out.Periods)
	assert.Empty(t, out.Days)
}
