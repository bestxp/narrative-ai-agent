package usecase

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"narrative/internal/adapter/storage"
)

func TestNPCManager_CreateNew(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	seedWorld(t, fs, "naruto")
	m := NewNPCManager(fs)
	err := m.Create("naruto", NPCProfile{
		DisplayName: "Какаши",
		File:        "Какаши",
		Temperament: "спокойный, ироничный",
		Relations:   "сенсей ГГ",
		Abilities:   "Шаринган, Чидори",
	})
	require.NoError(t, err)
	assert.True(t, fs.Exists("worlds/naruto/characters/kakashi.md"))
	reg, _ := fs.ReadRaw("worlds/naruto/characters.md")
	assert.Contains(t, reg, "Какаши")
}

func TestNPCManager_RejectsDuplicate(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	seedWorld(t, fs, "naruto")
	m := NewNPCManager(fs)
	p := NPCProfile{DisplayName: "X", File: "x", Temperament: "y"}
	require.NoError(t, m.Create("naruto", p))
	assert.ErrorIs(t, m.Create("naruto", p), ErrNPCExists)
}

func TestFilterKnowledge_StripsDisallowed(t *testing.T) {
	candidate := "Привет, как дела?<!NPC:greeting!>\n" +
		"Я знаю твою тайну — ты сын Хокаге.<!NPC:secret_origin!>\n"
	allowed := "<!NPC:greeting!>"
	got := FilterKnowledge(candidate, allowed)
	assert.NotContains(t, got, "тайну")
	assert.Contains(t, got, "Привет")
}

func TestFilterKnowledge_KeepsUnmarked(t *testing.T) {
	candidate := "Просто текст без маркеров."
	got := FilterKnowledge(candidate, "")
	assert.Equal(t, "Просто текст без маркеров.", got)
}

func TestNPCManager_Load(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	seedWorld(t, fs, "naruto")
	m := NewNPCManager(fs)
	require.NoError(t, m.Create("naruto", NPCProfile{DisplayName: "Саске", File: "sasuke"}))
	body, err := m.Load("naruto", "sasuke")
	require.NoError(t, err)
	assert.Contains(t, body, "Саске")
}

func TestNPCManager_Load_TransliteratesArg(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	seedWorld(t, fs, "naruto")
	m := NewNPCManager(fs)
	require.NoError(t, m.Create("naruto", NPCProfile{DisplayName: "Какаши", File: "kakashi"}))
	body, err := m.Load("naruto", "Какаши")
	require.NoError(t, err)
	assert.Contains(t, body, "Какаши")
}

func TestNPCManager_LogsOnCreate(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	seedWorld(t, fs, "naruto")
	log, buf := newBufLogger()
	m := NewNPCManagerWithLogger(fs, log)
	require.NoError(t, m.Create("naruto", NPCProfile{DisplayName: "Test", File: "test"}))
	assert.Contains(t, buf.String(), "npc_created")
}
