package summarizertools_test

import (
	"context"
	"testing"

	"github.com/bestxp/narrative-ai-agent/internal/adapter/summarizertools"
	"github.com/bestxp/narrative-ai-agent/internal/usecase"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildSlots_NilSummarizer(t *testing.T) {
	t.Parallel()

	slots := summarizertools.BuildSlots(nil)
	assert.False(t, slots.HasSummarizer)
	assert.Nil(t, slots.NPC)
	assert.Nil(t, slots.Lore)
	assert.Nil(t, slots.Chronicle)
	assert.Nil(t, slots.CharacterMem)
}

func TestBuildSlots_NonNilSummarizer(t *testing.T) {
	t.Parallel()

	slots := summarizertools.BuildSlots(&usecase.Summarizer{})
	assert.True(t, slots.HasSummarizer)
	assert.NotNil(t, slots.NPC)
	assert.NotNil(t, slots.Lore)
	assert.NotNil(t, slots.Chronicle)
	assert.NotNil(t, slots.CharacterMem)
}

func TestAdapter_PassThroughNPC(t *testing.T) {
	t.Parallel()

	// Unconfigured summarizer returns the input body,
	// no error. Verifies the contract surface.
	slots := summarizertools.BuildSlots(&usecase.Summarizer{})
	require.NotNil(t, slots.NPC)

	body, err := slots.NPC.SummarizeNPC(context.Background(), "name", "world", []byte("body"), []byte("tail"))
	require.NoError(t, err)
	assert.Equal(t, []byte("body"), body)
}

func TestAdapter_PassThroughLore(t *testing.T) {
	t.Parallel()

	slots := summarizertools.BuildSlots(&usecase.Summarizer{})
	require.NotNil(t, slots.Lore)

	body, err := slots.Lore.SummarizeLore(context.Background(), "world", []byte("lore"), []byte("tail"), []byte("state"))
	require.NoError(t, err)
	assert.Equal(t, []byte("lore"), body)
}

func TestAdapter_PassThroughChronicle(t *testing.T) {
	t.Parallel()

	slots := summarizertools.BuildSlots(&usecase.Summarizer{})
	require.NotNil(t, slots.Chronicle)

	body, err := slots.Chronicle.SummarizeChronicle(context.Background(), "world", 1, 30, "full")
	require.NoError(t, err)
	assert.Nil(t, body, "empty chronicle summariser returns nil body, caller treats as no-op")
}

func TestAdapter_PassThroughCharacterMemory(t *testing.T) {
	t.Parallel()

	slots := summarizertools.BuildSlots(&usecase.Summarizer{})
	require.NotNil(t, slots.CharacterMem)

	body, err := slots.CharacterMem.SummarizeCharacterMemory(context.Background(), "world", "char", []byte("mem"), []byte("tail"))
	require.NoError(t, err)
	assert.Equal(t, []byte("mem"), body)
}
