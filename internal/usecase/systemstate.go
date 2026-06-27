package usecase

import (
	"fmt"
	"time"

	"github.com/bestxp/narrative-ai-agent/internal/adapter/storage"
	"github.com/bestxp/narrative-ai-agent/internal/domain"
	"github.com/bestxp/narrative-ai-agent/internal/slowlog"
	"github.com/rs/zerolog"
)

// SystemState is the persistence layer for domain.SystemState.
// It is read on bot start to recover compaction history, written
// after every compaction and every successful autosave. The
// file is operator-facing — slowlog events keep a separate
// per-request audit so a corrupt or stale file never costs
// the player a turn.
type SystemState struct {
	fs   *storage.FileStore
	log  zerolog.Logger
	slow *slowlog.Logger
	// maxCompactionHistory caps the History slice so the file
	// stays small across long sessions. 50 ≈ 1kB of YAML,
	// negligible.
	maxCompactionHistory int
}

func NewSystemState(fs *storage.FileStore, log zerolog.Logger, slow *slowlog.Logger) *SystemState {
	return &SystemState{
		fs:                   fs,
		log:                  log.With().Str("component", "system_state").Logger(),
		slow:                 slow,
		maxCompactionHistory: 50,
	}
}

// Load reads the system_state.md file. A missing or empty file
// returns a zero SystemState — that is the "first run" case,
// not an error. Only structurally invalid YAML is an error,
// because silently dropping corrupt state would lose the
// audit trail.
func (s *SystemState) Load() (domain.SystemState, error) {
	body, err := s.fs.ReadRaw(domain.SystemStateFile)
	if err != nil {
		return domain.SystemState{}, fmt.Errorf("load: ReadRaw failed: %w", err)
	}

	if body == "" {
		return domain.DefaultSystemState(), nil
	}

	st, err := domain.ParseSystemState(body)
	if err != nil {
		return domain.SystemState{}, fmt.Errorf("load: parse: %w", err)
	}

	return st, nil
}

// Save writes the entire system_state.md body in one shot.
// It is called after Load+mutate when the changes are local
// (rare — the path is dominated by append-style updates below).
func (s *SystemState) Save(state domain.SystemState) error {
	body, err := state.MarshalSystemState()
	if err != nil {
		return fmt.Errorf("save: MarshalSystemState failed: %w", err)
	}

	if err := s.fs.WriteRawAtomic(domain.SystemStateFile, body); err != nil {
		return fmt.Errorf("save: WriteRawAtomic failed: %w", err)
	}

	s.log.Debug().Msg("system_state saved")

	return nil
}

// TouchSession updates the session block: bumps TurnCount /
// FreeformTurnCount and stamps LastActive. Other fields
// (Character, World, ChatID) are kept untouched.
func (s *SystemState) TouchSession(state *domain.SystemState, isFreeform bool, now time.Time) {
	state.Session.LastActive = now

	state.Session.TurnCount++
	if isFreeform {
		state.Session.FreeformTurnCount++
	}
}

// SetSessionContext sets Character/World/ChatID/StartedAt —
// called once on bot start with the values from info.yaml.
// Pass now for StartedAt (usually time.Now()).
func (s *SystemState) SetSessionContext(state *domain.SystemState, character, world, chatID string, now time.Time) {
	state.Session.StartedAt = now
	state.Session.Character = character
	state.Session.World = world
	state.Session.ChatID = chatID
}

// InitSession is a one-shot helper that loads the current
// system_state.md, calls SetSessionContext with the supplied
// (character, world, chatID) and writes the file back.
// Designed for cmd/bot/main.go to call once at startup.
// Returns the post-write SystemState so the caller can
// inspect the counters it just initialised.
func (s *SystemState) InitSession(character, world, chatID string, now time.Time) (domain.SystemState, error) {
	if s == nil {
		return domain.SystemState{}, nil
	}

	state, err := s.Load()
	if err != nil {
		return domain.SystemState{}, err
	}

	s.SetSessionContext(&state, character, world, chatID, now)

	if err := s.Save(state); err != nil {
		return domain.SystemState{}, err
	}

	return state, nil
}

// BumpSession is a per-message helper that loads the file,
// calls TouchSession and writes the file back. The isFreeform
// flag distinguishes freeform player turns from /-prefixed
// commands: only freeform turns count toward the
// FreeformTurnCount total (TurnCount counts every
// IncomingMessage).
//
// BumpSession is best-effort: a failure to read or write
// system_state.md logs a warning and returns nil. The
// player sees nothing — losing one audit row is much
// better than failing a chat reply.
func (s *SystemState) BumpSession(isFreeform bool, now time.Time) error {
	if s == nil {
		return nil
	}

	state, err := s.Load()
	if err != nil {
		return fmt.Errorf("bump_session: load: %w", err)
	}

	s.TouchSession(&state, isFreeform, now)

	if err := s.Save(state); err != nil {
		return fmt.Errorf("bump_session: save: %w", err)
	}

	return nil
}

// AppendCompaction is the canonical entry point for the
// compaction log. It loads the current file, appends the
// event (with the eviction logic in domain.CompactionLog),
// writes the file back, and emits a slowlog entry.
//
// The returned SystemState is the post-write state. The
// caller may keep using it for further mutations; if the
// caller wants to share state across calls, they should
// cache their own copy.
func (s *SystemState) AppendCompaction(ev domain.CompactionEvent) (domain.SystemState, error) {
	state, err := s.Load()
	if err != nil {
		return domain.SystemState{}, err
	}

	state.Compaction.AppendCompactionEvent(ev, s.maxCompactionHistory)

	if err := s.Save(state); err != nil {
		return domain.SystemState{}, err
	}

	s.log.Info().
		Time("at", ev.At).
		Str("trigger", ev.Trigger).
		Int("before", ev.BeforeTokens).
		Int("after", ev.AfterTokens).
		Int("dropped", ev.DroppedTurns).
		Msg("compaction logged")

	if s.slow != nil {
		_ = s.slow.Write("compaction", "", map[string]any{
			"trigger":       ev.Trigger,
			"role":          ev.Role,
			"before_tokens": ev.BeforeTokens,
			"after_tokens":  ev.AfterTokens,
			"dropped_turns": ev.DroppedTurns,
			"kept_recent":   ev.KeptRecent,
		})
	}

	return state, nil
}

// RecordAutosave stores the hash of the most recent commit and
// bumps the counter. Called from main.go's runAutoSave after
// every successful git commit (including empty ones — the
// counter reflects "save attempts" so the operator can see
// noise). For the player's "✅ сохранено" message, the bot
// also passes the empty flag so the runAutoSave code can
// short-circuit.
func (s *SystemState) RecordAutosave(hash string, now time.Time) (domain.SystemState, error) {
	state, err := s.Load()
	if err != nil {
		return domain.SystemState{}, err
	}

	if hash != "" {
		state.Autosave.LastHash = hash
		state.Autosave.LastSaveAt = now
		state.Autosave.TotalSaves++
	}

	if err := s.Save(state); err != nil {
		return domain.SystemState{}, err
	}

	if s.slow != nil && hash != "" {
		_ = s.slow.Write("autosave", "", map[string]any{
			"hash":        hash,
			"total_saves": state.Autosave.TotalSaves,
		})
	}

	return state, nil
}
