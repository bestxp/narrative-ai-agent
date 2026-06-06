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
	Warnings          []string
}

// ErrNoActiveSession is returned by Start when the registry is
// bootstrapped but has no active world. The operator can recover by
// running /launch.
var ErrNoActiveSession = errors.New("no active session: run /launch <character> <world>")

// ensureRegistry writes a minimal info.yaml placeholder when the
// registry is missing or empty. The placeholder has no active
// character or world; /launch fills them in. The placeholder exists
// so downstream code (PromptContext, /status, /push) can call
// ParseInfo without a nil-error special case.
func (s *SessionStart) ensureRegistry() error {
	if s.fs.Exists(storage.InfoFile) {
		raw, _ := s.fs.ReadRaw(storage.InfoFile)
		if raw != "" {
			return nil
		}
	}
	placeholder := domain.BuildInfo("", "", nil, nil)
	if err := s.fs.WriteRawAtomic(storage.InfoFile, placeholder); err != nil {
		s.log.Error().Err(err).Str("path", s.fs.InfoYAMLPath()).Msg("registry bootstrap failed")
		return fmt.Errorf("bootstrap %s: %w", storage.InfoFile, err)
	}
	s.log.Warn().Str("path", s.fs.InfoYAMLPath()).Msg("registry missing — created empty placeholder, run /launch")
	return nil
}

func (s *SessionStart) Start() (*SessionContext, error) {
	if err := s.ensureRegistry(); err != nil {
		return nil, err
	}
	infoRaw, err := s.fs.ReadRaw(storage.InfoFile)
	if err != nil {
		return nil, err
	}
	if infoRaw == "" {
		return nil, ErrNoActiveSession
	}
	info, err := domain.ParseInfo(infoRaw)
	if err != nil {
		return nil, fmt.Errorf("parse info.yaml: %w", err)
	}
	if info.ActiveWorld == "" {
		return nil, ErrNoActiveSession
	}
	world := info.ActiveWorld
	state, err := s.fs.ReadRaw("worlds/" + world + "/state.md")
	if err != nil {
		return nil, fmt.Errorf("read state: %w", err)
	}
	ctx := &SessionContext{
		Character: info.ActiveCharacter,
		World:     world,
		State:     state,
	}
	ctx.SyncStateAhead, ctx.SyncMemoriseAhead = s.checkSync(world, state)
	// Note: "session_start" is logged by the caller (gm.buildContextPrompt)
	// once per Reply, not here. ss.Start() is invoked from tool dispatch
	// (leave_world, update_character) as well, and we don't want a
	// duplicate line per tool call. The caller logs at info with the
	// full session context.
	return ctx, nil
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
