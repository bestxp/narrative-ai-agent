package chronicle_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/bestxp/narrative-ai-agent/internal/chronicle"
)

func TestLoad_Empty(t *testing.T) {
	t.Parallel()

	_, err := chronicle.Load("")
	if !errors.Is(err, chronicle.ErrNotFound) {
		t.Fatalf("expected chronicle.ErrNotFound, got %v", err)
	}
}

func TestLoad_WhitespaceOnly(t *testing.T) {
	t.Parallel()

	_, err := chronicle.Load("   \n\n  \n")
	if !errors.Is(err, chronicle.ErrNotFound) {
		t.Fatalf("expected chronicle.ErrNotFound, got %v", err)
	}
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
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	out, err := chronicle.Load(body)
	if err != nil {
		t.Fatalf("chronicle.Load: %v", err)
	}

	if len(out.Periods) != 2 {
		t.Fatalf("expected 2 periods, got %d", len(out.Periods))
	}

	if out.Periods[0].From != 1 || out.Periods[0].To != 30 || out.Periods[0].Memory != "first window summary" {
		t.Errorf("period[0]: got %+v", out.Periods[0])
	}

	if out.Periods[1].From != 31 || out.Periods[1].To != 60 || out.Periods[1].Memory != "second window" {
		t.Errorf("period[1]: got %+v", out.Periods[1])
	}

	if len(out.Days) != 2 {
		t.Fatalf("expected 2 days, got %d", len(out.Days))
	}

	if out.Days[61] != "raw day 61" {
		t.Errorf("day 61: got %q", out.Days[61])
	}

	if out.Days[62] != "raw day 62" {
		t.Errorf("day 62: got %q", out.Days[62])
	}
}

func TestLoad_OnlyPeriods(t *testing.T) {
	t.Parallel()

	body := `periods:
  - from: 1
    to: 30
    memory: "summary"
`

	c, err := chronicle.Load(body)
	if err != nil {
		t.Fatalf("chronicle.Load: %v", err)
	}

	if len(c.Periods) != 1 {
		t.Fatalf("expected 1 period, got %d", len(c.Periods))
	}

	if len(c.Days) != 0 {
		t.Errorf("expected empty days, got %d", len(c.Days))
	}
}

func TestLoad_OnlyDays(t *testing.T) {
	t.Parallel()

	body := `days:
  1: "raw"
  2: "raw 2"
`

	c, err := chronicle.Load(body)
	if err != nil {
		t.Fatalf("chronicle.Load: %v", err)
	}

	if len(c.Periods) != 0 {
		t.Errorf("expected empty periods, got %d", len(c.Periods))
	}

	if len(c.Days) != 2 {
		t.Errorf("expected 2 days, got %d", len(c.Days))
	}
}

func TestAppendDay_Basic(t *testing.T) {
	t.Parallel()

	c := chronicle.Chronicle{}
	if !c.AppendDay(1, "day 1") {
		t.Fatal("AppendDay should return true on first insert")
	}

	if !c.AppendDay(2, "day 2") {
		t.Fatal("AppendDay should return true for new day")
	}

	if c.AppendDay(1, "duplicate") {
		t.Fatal("AppendDay should return false for duplicate day")
	}

	if got := c.Days[1]; got != "day 1" {
		t.Errorf("day 1: got %q", got)
	}
}

func TestAppendDay_Trims(t *testing.T) {
	t.Parallel()

	c := chronicle.Chronicle{}
	c.AppendDay(1, "  hello  ")

	if got := c.Days[1]; got != "hello" {
		t.Errorf("expected trimmed text, got %q", got)
	}
}

func TestAppendDay_EmptyText(t *testing.T) {
	t.Parallel()

	c := chronicle.Chronicle{}
	if c.AppendDay(1, "") {
		t.Fatal("AppendDay should return false for empty text")
	}

	if c.AppendDay(1, "   ") {
		t.Fatal("AppendDay should return false for whitespace-only text")
	}
}

func TestAppendDay_ZeroDay(t *testing.T) {
	t.Parallel()

	c := chronicle.Chronicle{}
	if c.AppendDay(0, "x") {
		t.Fatal("AppendDay should return false for day=0")
	}

	if c.AppendDay(-1, "x") {
		t.Fatal("AppendDay should return false for negative day")
	}
}

func TestCompressWindow_Basic(t *testing.T) {
	t.Parallel()

	c := chronicle.Chronicle{Days: map[int]string{
		1: "a", 2: "b", 3: "c",
	}}
	if err := c.CompressWindow(1, 3, "compressed"); err != nil {
		t.Fatalf("CompressWindow: %v", err)
	}

	if len(c.Periods) != 1 {
		t.Fatalf("expected 1 period, got %d", len(c.Periods))
	}

	if c.Periods[0].From != 1 || c.Periods[0].To != 3 || c.Periods[0].Memory != "compressed" {
		t.Errorf("period: %+v", c.Periods[0])
	}

	if len(c.Days) != 0 {
		t.Errorf("expected days to be cleared, got %d: %+v", len(c.Days), c.Days)
	}
}

func TestCompressWindow_PreservesOutOfRangeDays(t *testing.T) {
	t.Parallel()

	c := chronicle.Chronicle{Days: map[int]string{
		1: "a", 2: "b", 3: "c", 4: "d", 5: "e",
	}}
	if err := c.CompressWindow(2, 4, "mid"); err != nil {
		t.Fatalf("CompressWindow: %v", err)
	}

	if len(c.Days) != 2 {
		t.Errorf("expected 2 surviving days, got %d: %+v", len(c.Days), c.Days)
	}

	if _, ok := c.Days[1]; !ok {
		t.Error("day 1 should survive")
	}

	if _, ok := c.Days[5]; !ok {
		t.Error("day 5 should survive")
	}
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
	if err == nil {
		t.Fatal("expected error on overlap, got nil")
	}

	if !strings.Contains(err.Error(), "inside closed period") {
		t.Errorf("expected 'inside closed period' error, got: %v", err)
	}
	// chronicle.Chronicle should be left untouched.
	if len(c.Periods) != 1 {
		t.Errorf("periods list should not change on error, got %d", len(c.Periods))
	}

	if len(c.Days) != 2 {
		t.Errorf("days list should not change on error, got %d", len(c.Days))
	}
}

func TestCompressWindow_InvalidRange(t *testing.T) {
	t.Parallel()

	c := chronicle.Chronicle{}
	if err := c.CompressWindow(10, 5, "x"); err == nil {
		t.Error("expected error when from > to")
	}

	if err := c.CompressWindow(0, 5, "x"); err == nil {
		t.Error("expected error when from = 0")
	}
}

func TestCompressWindow_EmptyMemory(t *testing.T) {
	t.Parallel()

	c := chronicle.Chronicle{}
	if err := c.CompressWindow(1, 5, ""); err == nil {
		t.Error("expected error on empty memory")
	}

	if err := c.CompressWindow(1, 5, "   "); err == nil {
		t.Error("expected error on whitespace-only memory")
	}
}

func TestLastDay(t *testing.T) {
	t.Parallel()

	c := chronicle.Chronicle{}
	if _, ok := c.LastDay(); ok {
		t.Error("LastDay should be false on empty chronicle")
	}

	c.Days = map[int]string{1: "a", 5: "b", 3: "c"}

	got, ok := c.LastDay()
	if !ok || got != 5 {
		t.Errorf("LastDay: got (%d, %v), want (5, true)", got, ok)
	}
}

func TestLastPeriodEnd(t *testing.T) {
	t.Parallel()

	c := chronicle.Chronicle{}
	if _, ok := c.LastPeriodEnd(); ok {
		t.Error("LastPeriodEnd should be false on empty chronicle")
	}

	c.Periods = []chronicle.Period{
		{From: 1, To: 30, Memory: "a"},
		{From: 31, To: 45, Memory: "b"},
		{From: 46, To: 60, Memory: "c"},
	}

	got, ok := c.LastPeriodEnd()
	if !ok || got != 60 {
		t.Errorf("LastPeriodEnd: got (%d, %v), want (60, true)", got, ok)
	}
}

func TestSortedDays(t *testing.T) {
	t.Parallel()

	c := chronicle.Chronicle{Days: map[int]string{
		3: "c", 1: "a", 2: "b",
	}}
	out := c.SortedDays()
	want := []int{1, 2, 3}

	if len(out) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(out))
	}

	for i, n := range want {
		if out[i].Number != n {
			t.Errorf("entry %d: got %d, want %d", i, out[i].Number, n)
		}
	}
}

func TestSave_EmptyChronicleHasBothSections(t *testing.T) {
	t.Parallel()

	c := chronicle.Chronicle{}

	body, err := c.Save()
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	if !strings.Contains(body, "periods:") {
		t.Errorf("expected 'periods:' in body, got:\n%s", body)
	}

	if !strings.Contains(body, "days:") {
		t.Errorf("expected 'days:' in body, got:\n%s", body)
	}
}

func TestSaveLoad_EmptyChronicle(t *testing.T) {
	t.Parallel()

	c := chronicle.Chronicle{}

	body, err := c.Save()
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	out, err := chronicle.Load(body)
	if err != nil {
		t.Fatalf("chronicle.Load: %v", err)
	}

	if len(out.Periods) != 0 {
		t.Errorf("expected 0 periods, got %d", len(out.Periods))
	}

	if len(out.Days) != 0 {
		t.Errorf("expected 0 days, got %d", len(out.Days))
	}
}

func TestWindowSize_MatchesLimits(t *testing.T) {
	t.Parallel()
	// Sanity: the local alias must agree with the
	// single source of truth in internal/limits. If
	// they drift, the bot's compression rule silently
	// changes and operators see no warning.
	if chronicle.WindowSize != 30 {
		t.Errorf("chronicle.WindowSize = %d, want 30 (limits.MemoriseWindowDays)", chronicle.WindowSize)
	}
}
