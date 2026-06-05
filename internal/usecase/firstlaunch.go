package usecase

import (
	"errors"
	"fmt"
	"strings"

	"github.com/rs/zerolog"

	"narrative/internal/adapter/storage"
	"narrative/internal/domain"
)

// FirstLaunch creates the entire on-disk skeleton for a new player +
// world pair. It is idempotent: if info.md already exists and points
// at a different character, an error is returned and nothing is touched.
type FirstLaunch struct {
	fs  *storage.FileStore
	log zerolog.Logger
}

func NewFirstLaunch(fs *storage.FileStore) *FirstLaunch {
	return NewFirstLaunchWithLogger(fs, zerolog.Nop())
}

func NewFirstLaunchWithLogger(fs *storage.FileStore, log zerolog.Logger) *FirstLaunch {
	return &FirstLaunch{fs: fs, log: log.With().Str("component", "first_launch").Logger()}
}

type CharacterSpec struct {
	DisplayName string // human-readable
	Dir         string // latin, sanitised
	TrueNature  string
	Philosophy  string
}

type WorldSpec struct {
	DisplayName string
	Dir         string
	IsKnown     bool
	Canon       string // для известного — справочник, для выдуманного — сценарий
}

var (
	ErrAlreadyLaunched = errors.New("first launch: game-data/info.md already exists")
	ErrInvalidSpec     = errors.New("first launch: invalid character or world name")
)

func (f *FirstLaunch) Launch(char CharacterSpec, world WorldSpec) error {
	if f.fs.Exists("info.md") {
		return ErrAlreadyLaunched
	}
	charDir, err := domain.SanitizeName(char.Dir)
	if err != nil {
		return fmt.Errorf("character dir: %w", err)
	}
	worldDir, err := domain.SanitizeName(world.Dir)
	if err != nil {
		return fmt.Errorf("world dir: %w", err)
	}
	if err := f.writeCharacter(charDir, char); err != nil {
		return err
	}
	if err := f.writeWorld(worldDir, world); err != nil {
		return err
	}
	if err := f.fs.WriteRawAtomic("info.md", domain.BuildInfo(charDir, worldDir, nil, nil)); err != nil {
		return err
	}
	f.log.Info().Str("character", charDir).Str("world", worldDir).Msg("first_launch")
	return nil
}

func (f *FirstLaunch) writeCharacter(dir string, c CharacterSpec) error {
	root := "characters/" + dir
	if err := f.fs.EnsureDir(root); err != nil {
		return err
	}
	soul := "# " + strings.TrimSpace(c.DisplayName) + " — Ядро персонажа\n" +
		"## Истинная сущность\n" + strings.TrimSpace(c.TrueNature) + "\n\n" +
		"## Философия и принципы\n" + strings.TrimSpace(c.Philosophy) + "\n"
	if err := f.fs.WriteRawAtomic(root+"/SOUL.md", soul); err != nil {
		return err
	}
	skill := "# Способности " + strings.TrimSpace(c.DisplayName) + "\n" +
		"## Оружие\n\n## Базовые способности\n\n## Фундаментальные стихии\n\n## Особые проявления\n\n## Универсальные навыки\n\n## Ограничения\n"
	if err := f.fs.WriteRawAtomic(root+"/SKILL.md", skill); err != nil {
		return err
	}
	mem := "# Яркие воспоминания " + strings.TrimSpace(c.DisplayName) + "\n> Субъективные моменты. От первого лица.\n"
	return f.fs.WriteRawAtomic(root+"/memory.md", mem)
}

func (f *FirstLaunch) writeWorld(dir string, w WorldSpec) error {
	root := "worlds/" + dir
	if err := f.fs.EnsureDir(root + "/characters"); err != nil {
		return err
	}
	canon := "# " + strings.TrimSpace(w.DisplayName) + " — канон/сценарий\n" + strings.TrimSpace(w.Canon) + "\n"
	if err := f.fs.WriteRawAtomic(root+"/canon.md", canon); err != nil {
		return err
	}
	state := StateHeader(1, true) + "\nСтартовая сцена.\n"
	if err := f.fs.WriteRawAtomic(root+"/state.md", state); err != nil {
		return err
	}
	lore := "# Мир " + strings.TrimSpace(w.DisplayName) + "\nКанон актуален, если игрок не вносит изменения.\n"
	if err := f.fs.WriteRawAtomic(root+"/lore.md", lore); err != nil {
		return err
	}
	if err := NewMaintenance(f.fs).RotatePlan(dir, []string{
		"вводная сцена: знакомство с миром",
		"первая зацепка / конфликт",
		"первая развилка",
	}); err != nil {
		return err
	}
	if err := f.fs.WriteRawAtomic(root+"/memorise.md", ""); err != nil {
		return err
	}
	reg := "# NPC: " + strings.TrimSpace(w.DisplayName) + "\n| Имя | Файл | Прозвища |\n|-----|------|----------|\n"
	return f.fs.WriteRawAtomic(root+"/characters.md", reg)
}
