package usecase

import (
	"errors"
	"fmt"
	"strings"

	"github.com/rs/zerolog"

	"narrative/internal/adapter/storage"
	"narrative/internal/domain"
)

// SessionStart implements the "SESSION START PROTOCOL" from the skill.
type SessionStart struct {
	fs  *storage.FileStore
	log zerolog.Logger
}

func NewSessionStart(fs *storage.FileStore) *SessionStart {
	return NewSessionStartWithLogger(fs, zerolog.Nop())
}

func NewSessionStartWithLogger(fs *storage.FileStore, log zerolog.Logger) *SessionStart {
	return &SessionStart{fs: fs, log: log.With().Str("component", "session_start").Logger()}
}

type SessionContext struct {
	Character         string
	World             string
	State             string
	SyncStateAhead    bool
	SyncMemoriseAhead bool
	UnreadAnchors     []string
	Warnings          []string
}

var ErrNoActiveSession = errors.New("no active session: run /start to create one")

func (s *SessionStart) Start() (*SessionContext, error) {
	infoRaw, err := s.fs.ReadRaw("info.md")
	if err != nil {
		return nil, err
	}
	if infoRaw == "" {
		return nil, ErrNoActiveSession
	}
	charRef, worldRef, err := domain.ParseInfo(infoRaw)
	if err != nil {
		return nil, fmt.Errorf("parse info: %w", err)
	}
	if !worldRef.Active {
		return nil, errors.New("info.md: no active world")
	}
	world := strings.TrimPrefix(worldRef.Pointer, "worlds/")
	state, err := s.fs.ReadRaw("worlds/" + world + "/state.md")
	if err != nil {
		return nil, fmt.Errorf("read state: %w", err)
	}
	ctx := &SessionContext{
		Character: strings.TrimPrefix(charRef.Pointer, "characters/"),
		World:     world,
		State:     state,
	}
	anchors, warns := s.scanAnchors(infoRaw)
	ctx.UnreadAnchors = anchors
	ctx.Warnings = append(ctx.Warnings, warns...)
	ctx.SyncStateAhead, ctx.SyncMemoriseAhead = s.checkSync(world, state)
	s.log.Info().Str("world", world).Str("character", ctx.Character).Int("unread_anchors", len(anchors)).Msg("session_start")
	return ctx, nil
}

// scanAnchors returns the unchecked anchors from the "## Правила"
// section. Anchor lines look like `- [ ] Не управляю...`.
func (s *SessionStart) scanAnchors(info string) (unchecked []string, warns []string) {
	inRules := false
	for _, line := range strings.Split(info, "\n") {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "##") {
			inRules = strings.Contains(strings.ToLower(trim), "правил")
			continue
		}
		if !inRules {
			continue
		}
		if strings.HasPrefix(trim, "- [ ]") {
			unchecked = append(unchecked, strings.TrimSpace(strings.TrimPrefix(trim, "- [ ]")))
		}
	}
	if len(unchecked) > 0 {
		warns = append(warns, fmt.Sprintf("непрочитанных якорей: %d", len(unchecked)))
	}
	return
}

// checkSync compares the day counter in state.md with the last archived
// day in memorise.md. If state is ahead, returns (true, false) — caller
// must backfill. If memorise is ahead of state, returns (false, true)
// — caller must surface a hallucination warning to the player.
func (s *SessionStart) checkSync(world, state string) (stateAhead, memoriseAhead bool) {
	stateDay, stateOK := extractDayNumber(state)
	memRaw, _ := s.fs.ReadRaw("worlds/" + world + "/memorise.md")
	memDay, memOK := domain.LastDay(memRaw)
	switch {
	case !stateOK && !memOK:
		return false, false
	case !stateOK && memOK:
		return false, true
	case stateOK && !memOK:
		return true, false
	case stateDay > memDay:
		return true, false
	case memDay > stateDay:
		return false, true
	}
	return false, false
}

var dayHeaderRe = extractDayHeaderRegex()

func extractDayHeaderRegex() func(string) (int, bool) {
	// matches "День N (" at the very start of state.md
	return func(s string) (int, bool) {
		idx := strings.Index(s, "День ")
		if idx < 0 {
			return 0, false
		}
		rest := s[idx+len("День "):]
		end := 0
		for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
			end++
		}
		if end == 0 {
			return 0, false
		}
		n := 0
		for i := 0; i < end; i++ {
			n = n*10 + int(rest[i]-'0')
		}
		return n, true
	}
}

func extractDayNumber(s string) (int, bool) {
	return dayHeaderRe(s)
}
