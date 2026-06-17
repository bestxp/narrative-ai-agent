package files

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/rs/zerolog"

	"github.com/bestxp/narrative-ai-agent/internal/adapter/storage"
	"github.com/bestxp/narrative-ai-agent/internal/domain"
	"github.com/bestxp/narrative-ai-agent/internal/usecase/tools"
)

// World is the file-backed implementation of tools.WorldTool:
// /leave (active world switch) and /return (time-skip into a
// parked world).
type World struct {
	fs  *storage.FileStore
	log zerolog.Logger
	// worldStateInvalidate is called when the active world
	// changes (Leave hook). Wired by SetWorldStateInvalidate
	// from main.go.
	worldStateInvalidate func(reason string)
}

func newWorld(fs *storage.FileStore, log zerolog.Logger) *World {
	return &World{fs: fs, log: log.With().Str("component", "world").Logger()}
}

// SetWorldStateInvalidate wires the post-Leave hook.
func (w *World) SetWorldStateInvalidate(fn func(reason string)) {
	w.worldStateInvalidate = fn
}

// Leave switches the active world. If the new world does not exist on
// disk yet, it is initialised with sensible defaults. The skipped
// amount of time in the old world is provided by the player; "" means
// "an instant".
func (w *World) Leave(fromWorld, toWorld, skipNote, character string) (*tools.LeaveResult, error) {
	from, err := domain.SanitizeName(fromWorld)
	if err != nil {
		return nil, fmt.Errorf("from: %w", err)
	}
	to, err := domain.SanitizeName(toWorld)
	if err != nil {
		return nil, fmt.Errorf("to: %w", err)
	}
	fromState, err := w.fs.ReadRaw("worlds/" + from + "/state.md")
	if err != nil {
		return nil, fmt.Errorf("read from state: %w", err)
	}
	fromDay, _ := extractDayNumber(fromState)
	if skipNote == "" {
		skipNote = "мгновение"
	}
	note := fmt.Sprintf("Уход в мир %s. Прошло времени: %s.", to, skipNote)
	if err := w.fs.WriteRawAtomic("worlds/"+from+"/state.md",
		StateHeader(fromDay, false)+"\n"+note+"\n"); err != nil {
		return nil, err
	}
	planRaw, _ := w.fs.ReadRaw("worlds/" + from + "/plan.md")
	if planRaw != "" && !strings.Contains(planRaw, "[заморожено]") {
		planRaw += "\n[заморожено: переход в " + to + "]\n"
		_ = w.fs.WriteRawAtomic("worlds/"+from+"/plan.md", planRaw)
	}
	created := false
	if !w.fs.Exists("worlds/" + to) {
		if err := w.initialiseBlankWorld(to); err != nil {
			return nil, err
		}
		created = true
	}
	if err := w.switchActive(to, character); err != nil {
		return nil, err
	}
	if character != "" {
		// Append a one-line memory entry to the
		// character that just left the world. We use
		// the in-package newMemory constructor with
		// nil summarizers — AppendMemory does not
		// need the LLM, and creating a Memory on
		// every Leave is cheap (no state besides
		// fs + log + nil summarizer).
		_ = newMemory(w.fs, w.log, nil, nil, nil, nil).AppendMemory(character,
			"Переход в мир "+to+". "+skipNote+".")
	}
	w.log.Info().Str("from", from).Str("to", to).Bool("new_world", created).Int("from_day", fromDay).Msg("world_leave")
	// World change = new scene. Drop the cached WorldState
	// (different world, different character, different day —
	// the cache key in GM.sceneKeyOf would already miss on
	// next build, but we invalidate proactively to free
	// memory and to trigger the slowlog event so an operator
	// can see the transition in audit).
	if w.worldStateInvalidate != nil {
		w.worldStateInvalidate("leave_world")
	}
	return &tools.LeaveResult{FromWorld: from, FromDay: fromDay, NewWorld: to, NewWorldInit: created}, nil
}

func (w *World) switchActive(toWorld, character string) error {
	body, err := w.fs.ReadRaw(storage.InfoFile)
	if err != nil {
		w.log.Error().
			Err(err).
			Str("path", w.fs.InfoYAMLPath()).
			Str("to_world", toWorld).
			Msg("registry read failed — was /launch run?")
		return fmt.Errorf("read %s: %w", storage.InfoFile, err)
	}
	info, err := domain.ParseInfo(body)
	if err != nil {
		return err
	}
	if character != "" {
		info.ActiveCharacter = character
	}
	info.ActiveWorld = toWorld
	found := false
	for _, x := range info.Worlds {
		if x == toWorld {
			found = true
			break
		}
	}
	if !found {
		info.Worlds = append(info.Worlds, toWorld)
	}
	rendered := domain.BuildInfo(info.ActiveCharacter, info.ActiveWorld, without(info.Characters, info.ActiveCharacter), without(info.Worlds, info.ActiveWorld))
	return w.fs.WriteRawAtomic(storage.InfoFile, rendered)
}

func without(xs []string, drop string) []string {
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		if x != drop {
			out = append(out, x)
		}
	}
	return out
}

func (w *World) initialiseBlankWorld(dir string) error {
	root := "worlds/" + dir
	if err := w.fs.EnsureDir(root + "/characters"); err != nil {
		return err
	}
	for _, p := range []struct{ rel, body string }{
		{root + "/canon.md", "# " + dir + " — канон/сценарий\n"},
		{root + "/state.md", StateHeader(1, true) + "\nСтартовая сцена.\n"},
		{root + "/lore.md", "# Мир " + dir + "\nКанон актуален, если игрок не вносит изменения.\n"},
		{root + "/chronicle.yaml", ""},
		{root + "/characters.md", "# NPC: " + dir + "\n| Имя | Файл | Прозвища |\n|-----|------|----------|\n"},
	} {
		if err := w.fs.WriteRawAtomic(p.rel, p.body); err != nil {
			return err
		}
	}
	return newState(w.fs, w.log, nil).RotatePlan(dir, []string{
		"вводная сцена: знакомство с миром",
		"первая зацепка / конфликт",
		"первая развилка",
	})
}

// ReturnWorld applies a literal time-skip to the left world and
// prepares a re-entry scene description.
func (w *World) ReturnWorld(world, days string) (string, error) {
	wDir, err := domain.SanitizeName(world)
	if err != nil {
		return "", err
	}
	d, err := strconv.Atoi(strings.TrimSpace(days))
	if err != nil {
		return "", fmt.Errorf("days must be integer: %w", err)
	}
	if d < 0 {
		return "", errors.New("days must be non-negative")
	}
	stateRaw, _ := w.fs.ReadRaw("worlds/" + wDir + "/state.md")
	cur, _ := extractDayNumber(stateRaw)
	note := fmt.Sprintf("Возврат в мир %s. Прошло %d дн. с последней записи (день %d).",
		wDir, d, cur)
	newState := StateHeader(cur+d, true) + "\n" + note + "\n"
	if err := w.fs.WriteRawAtomic("worlds/"+wDir+"/state.md", newState); err != nil {
		return "", err
	}
	if err := w.switchActive(wDir, ""); err != nil {
		return "", err
	}
	w.log.Info().Str("world", wDir).Int("days", d).Int("new_day", cur+d).Msg("world_return")
	return note, nil
}
