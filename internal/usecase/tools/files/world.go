package files

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/rs/zerolog"

	"github.com/bestxp/narrative-ai-agent/internal/chronicle"
	"github.com/bestxp/narrative-ai-agent/internal/domain"
	"github.com/bestxp/narrative-ai-agent/internal/repository/api"
	"github.com/bestxp/narrative-ai-agent/internal/repository/yaml"
	"github.com/bestxp/narrative-ai-agent/internal/usecase/tools"
)

// World is the repository-backed implementation of
// tools.WorldTool: /leave (active world switch) and
// /return (time-skip into a parked world).
//
// All persistent reads and writes go through
// *api.Repositories.
type World struct {
	repos *api.Repositories
	log   zerolog.Logger
	// worldStateInvalidate is called when the active
	// world changes (Leave hook). Wired by
	// SetWorldStateInvalidate from main.go.
	worldStateInvalidate func(reason string)
}

func newWorld(log zerolog.Logger, repos *api.Repositories) *World {
	return &World{repos: repos, log: log.With().Str("component", "world").Logger()}
}

// SetWorldStateInvalidate wires the post-Leave hook.
func (w *World) SetWorldStateInvalidate(fn func(reason string)) {
	w.worldStateInvalidate = fn
}

// Leave switches the active world.
func (w *World) Leave(fromWorld, toWorld, skipNote, character string) (*tools.LeaveResult, error) {
	from, err := domain.SanitizeName(fromWorld)
	if err != nil {
		return nil, fmt.Errorf("from: %w", err)
	}
	to, err := domain.SanitizeName(toWorld)
	if err != nil {
		return nil, fmt.Errorf("to: %w", err)
	}
	fromSnap, err := w.repos.WorldState.Load(from)
	if err != nil {
		return nil, fmt.Errorf("read from state: %w", err)
	}
	fromDay := fromSnap.Day
	if skipNote == "" {
		skipNote = "мгновение"
	}
	note := fmt.Sprintf("Уход в мир %s. Прошло времени: %s.", to, skipNote)
	fromSnap.InFlight = false
	fromSnap.Moment = note
	if err := w.repos.WorldState.Save(from, fromSnap); err != nil {
		return nil, err
	}
	// Freeze plan.
	planRaw, _ := w.repos.Plan.Load(from)
	if planRaw != "" && !strings.Contains(planRaw, "[заморожено]") {
		planRaw += "\n[заморожено: переход в " + to + "]\n"
		_ = w.repos.Plan.Save(from, planRaw)
	}
	// Initialise blank world if needed.
	created := false
	canon, _ := w.repos.Canon.Load(to)
	if canon == "" {
		if err := w.initialiseBlankWorld(to); err != nil {
			return nil, err
		}
		created = true
	}
	if err := w.switchActive(to, character); err != nil {
		return nil, err
	}
	w.log.Info().Str("from", from).Str("to", to).Bool("new_world", created).Int("from_day", fromDay).Msg("world_leave")
	if w.worldStateInvalidate != nil {
		w.worldStateInvalidate("leave_world")
	}
	return &tools.LeaveResult{FromWorld: from, FromDay: fromDay, NewWorld: to, NewWorldInit: created}, nil
}

func (w *World) switchActive(toWorld, character string) error {
	info, err := w.repos.Info.Load()
	if err != nil {
		return fmt.Errorf("read info: %w", err)
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
	return w.repos.Info.Save(info)
}

func (w *World) initialiseBlankWorld(dir string) error {
	// Canon.

	// State.
	if err := w.repos.WorldState.EnsureExists(dir, 1, true); err != nil {
		return err
	}
	// Lore.
	if err := w.repos.Lore.Save(dir, "# Мир "+dir+"\nКанон актуален, если игрок не вносит изменения.\n"); err != nil {
		return err
	}
	// Chronicle — empty skeleton.
	c := newEmptyChronicle()
	if err := w.repos.Chronicle.Save(dir, c); err != nil {
		return err
	}
	// NPC registry (worlds/<dir>/characters.yaml) is NOT
	// seeded here — the registry is owned by the
	// worldregistry package and is created lazily on the
	// first create_npc call. An empty world has no
	// characters until the GM introduces them.
	// Plan — 3 default events.
	return w.repos.Plan.ReplaceEvents(context.Background(), dir, []string{
		"вводная сцена: знакомство с миром",
		"первая зацепка / конфликт",
		"первая развилка",
	})
}

// newEmptyChronicle returns a Chronicle with both
// arrays initialised (not nil) so Save() round-trips
// to "periods: []\ndays: {}".
func newEmptyChronicle() chronicle.Chronicle {
	return chronicle.Chronicle{
		Periods: []chronicle.Period{},
		Days:    map[int]string{},
	}
}

// chronicleChronicle and chroniclePeriod are local
// aliases to avoid importing the chronicle package
// directly (the import is already present via the
// repository layer). We use the same struct shape.

// ReturnWorld applies a time-skip and re-enters a
// parked world.
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
	snap, err := w.repos.WorldState.Load(wDir)
	if err != nil {
		return "", err
	}
	cur := snap.Day
	note := fmt.Sprintf("Возврат в мир %s. Прошло %d дн. с последней записи (день %d).",
		wDir, d, cur)
	snap.Day = cur + d
	snap.InFlight = true
	snap.Moment = note
	if err := w.repos.WorldState.Save(wDir, snap); err != nil {
		return "", err
	}
	if err := w.switchActive(wDir, ""); err != nil {
		return "", err
	}
	w.log.Info().Str("world", wDir).Int("days", d).Int("new_day", cur+d).Msg("world_return")
	return note, nil
}

// yaml is imported to keep the render helper available
// for state.md rendering within this package.
var _ = yaml.RenderStateBody
