package domain

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFormatAndParse(t *testing.T) {
	body := "Header\nд00001: первый день\nд00042: второй\n"
	entries, err := ParseDays(body)
	assert.NoError(t, err)
	require.Len(t, entries, 2)
	assert.Equal(t, 1, entries[0].Number)
	assert.Equal(t, "первый день", entries[0].Text)
	assert.Equal(t, 42, entries[1].Number)
}

func TestLastDay(t *testing.T) {
	_, ok := LastDay("")
	assert.False(t, ok)
	body := "д00001: x\nд00007: y\n"
	n, ok := LastDay(body)
	assert.True(t, ok)
	assert.Equal(t, 7, n)
}

func TestFormatDay(t *testing.T) {
	assert.Equal(t, "д00003: событие", FormatDay(3, "событие"))
}

func TestParseDays_IgnoresShortDayFormat(t *testing.T) {
	body := "д0000X: broken\nд00001: valid"
	entries, err := ParseDays(body)
	assert.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, 1, entries[0].Number)
}
