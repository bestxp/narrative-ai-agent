package usecase

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/rs/zerolog"

	"narrative/internal/adapter/storage"
	"narrative/internal/domain"
)

// WorldTransition implements "Покидаем мир" and "Возвращение в мир"
// from the skill.
type WorldTransition struct {
	fs  *storage.FileStore
	log zerolog.Logger
}

func NewWorldTransition(fs *storage.FileStore) *WorldTransition {
	return NewWorldTransitionWithLogger(fs, zerolog.Nop())
}

func NewWorldTransitionWithLogger(fs *storage.FileStore, log zerolog.Logger) *WorldTransition {
	return &WorldTransition{fs: fs, log: log.With().Str("component", "world_transition").Logger()}
}

type LeaveResult struct {
	FromWorld    string
	FromDay      int
	NewWorld     string
	NewWorldInit bool
}

// Leave switches the active world. If the new world does not exist on
// disk yet, it is initialised with sensible defaults. The skipped
// amount of time in the old world is provided by the player; "" means
// "an instant".
func (w *WorldTransition) Leave(fromWorld, toWorld, skipNote, character string) (*LeaveResult, error) {
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
	// Compress current state to a short departure note.
	note := fmt.Sprintf("Уход в мир %s. Прошло времени: %s.", to, skipNote)
	if err := w.fs.WriteRawAtomic("worlds/"+from+"/state.md",
		StateHeader(fromDay, false)+"\n"+note+"\n"); err != nil {
		return nil, err
	}
	// Freeze plan.md: append a single comment line so it remains forward-only.
	planRaw, _ := w.fs.ReadRaw("worlds/" + from + "/plan.md")
	if planRaw != "" && !strings.Contains(planRaw, "[заморожено]") {
		planRaw += "\n[заморожено: переход в " + to + "]\n"
		_ = w.fs.WriteRawAtomic("worlds/"+from+"/plan.md", planRaw)
	}
	// Initialise new world if absent.
	created := false
	if !w.fs.Exists("worlds/" + to) {
		if err := w.initialiseBlankWorld(to); err != nil {
			return nil, err
		}
		created = true
	}
	// Switch active world in info.md.
	if err := w.switchActive(to, character); err != nil {
		return nil, err
	}
	if character != "" {
		_ = NewMaintenanceWithLogger(w.fs, w.log).AppendMemory(character,
			"Переход в мир "+to+". "+skipNote+".")
	}
	w.log.Info().Str("from", from).Str("to", to).Bool("new_world", created).Int("from_day", fromDay).Msg("world_leave")
	return &LeaveResult{FromWorld: from, FromDay: fromDay, NewWorld: to, NewWorldInit: created}, nil
}

func (w *WorldTransition) switchActive(toWorld, character string) error {
	cur, _ := w.fs.ReadRaw("info.md")
	if cur == "" {
		return errors.New("info.md not found")
	}
	lines := strings.Split(cur, "\n")
	var inActiveWorld, inWorldsSection bool
	for i, ln := range lines {
		trim := strings.TrimSpace(ln)
		if strings.HasPrefix(trim, "## ") {
			inActiveWorld = trim == "## Активный мир"
			inWorldsSection = trim == "## Миры"
			continue
		}
		if !strings.HasPrefix(trim, "- [") {
			continue
		}
		if inActiveWorld && strings.Contains(ln, "worlds/") {
			lines[i] = "- [АКТИВЕН] worlds/" + toWorld
			inActiveWorld = false
			continue
		}
		if inWorldsSection && strings.Contains(ln, "worlds/"+toWorld) {
			lines[i] = "- [АКТИВЕН] worlds/" + toWorld
		}
	}
	// If bleach did not exist in info.md at all, inject into Миры.
	if !containsWorld(lines, toWorld) {
		// Find the "## Миры" section and append.
		for i, ln := range lines {
			if strings.TrimSpace(ln) == "## Миры" {
				lines = append(lines[:i+1], append([]string{"- [АКТИВЕН] worlds/" + toWorld}, lines[i+1:]...)...)
				break
			}
		}
	}
	return w.fs.WriteRawAtomic("info.md", strings.Join(lines, "\n"))
}

func containsWorld(lines []string, w string) bool {
	for _, ln := range lines {
		if strings.Contains(ln, "worlds/"+w) {
			return true
		}
	}
	return false
}

func (w *WorldTransition) initialiseBlankWorld(dir string) error {
	root := "worlds/" + dir
	if err := w.fs.EnsureDir(root + "/characters"); err != nil {
		return err
	}
	for _, p := range []struct{ rel, body string }{
		{root + "/canon.md", "# " + dir + " — канон/сценарий\n"},
		{root + "/state.md", StateHeader(1, true) + "\nСтартовая сцена.\n"},
		{root + "/lore.md", "# Мир " + dir + "\nКанон актуален, если игрок не вносит изменения.\n"},
		{root + "/memorise.md", ""},
		{root + "/characters.md", "# NPC: " + dir + "\n| Имя | Файл | Прозвища |\n|-----|------|----------|\n"},
	} {
		if err := w.fs.WriteRawAtomic(p.rel, p.body); err != nil {
			return err
		}
	}
	return NewMaintenanceWithLogger(w.fs, w.log).RotatePlan(dir, []string{
		"вводная сцена: знакомство с миром",
		"первая зацепка / конфликт",
		"первая развилка",
	})
}

// ReturnWorld applies a literal time-skip to the left world and
// prepares a re-entry scene description.
func (w *WorldTransition) ReturnWorld(world, days string) (string, error) {
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
