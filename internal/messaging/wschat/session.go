package wschat

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/bestxp/narrative-ai-agent/internal/structured"
	"github.com/gorilla/websocket"
	"github.com/rs/zerolog"
)

// wsSession is one active WebSocket connection. It owns the
// per-connection write pump (so frames from the server side are
// serialised on the wire) and tracks the in-flight LLM turn so
// edit_last / resend_last arriving while the model is streaming
// are rejected cleanly.
type wsSession struct {
	conn   *websocket.Conn
	chatID string
	log    zerolog.Logger

	// writeCh is fed by send(); the write pump drains it. Closing
	// the channel signals the pump to exit.
	writeCh   chan Frame
	closeOnce sync.Once

	// turn tracks whether an LLM turn is in flight. startTurn
	// returns false when one is already running.
	turnMtx      sync.Mutex
	turnInFlight bool
	// currentTurnID is the id of the active turn. It is copied
	// onto every frame that belongs to the turn (delta, status,
	// message, ack) so the client can group frames by turn and
	// avoid cross-turn replacement. Set when startTurn is called
	// with an id.
	currentTurnID string

	// finalText is the accumulated assistant text for the current
	// turn. The server reads it via finalizeText after the turn
	// completes.
	bufMtx   sync.Mutex
	buf      strings.Builder
	jsonMode bool
	textSeen bool

	// tokens accumulates the per-round usage the GM reports via
	// OnTokens. The server reads it at turn-final time so the
	// final assistant message can carry a token summary.
	tokensMtx sync.Mutex
	tokens    TokenUsage
}

func newSession(conn *websocket.Conn, chatID string, log zerolog.Logger) *wsSession {
	return &wsSession{
		conn:    conn,
		chatID:  chatID,
		log:     log,
		writeCh: make(chan Frame, 64),
	}
}

// run starts the read and write loops. It blocks until both exit.
// The server-side ctx is used to break out on shutdown.
func (s *wsSession) run(ctx context.Context, srv *Server) {
	// Write pump.
	go s.writePump(ctx)

	// Read pump.
	defer s.close()

	for {
		_, raw, err := s.conn.ReadMessage()
		if err != nil {
			if !websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				s.log.Debug().Err(err).Msg("ws read ended")
			}

			return
		}
		var f Frame
		if err := json.Unmarshal(raw, &f); err != nil {
			s.log.Warn().Err(err).Msg("ws bad frame")
			continue
		}
		srv.handleClientFrame(ctx, s, f)
	}
}

// send queues a frame for the write pump. Non-blocking: when the
// write channel is full the frame is dropped (dev: better to lose a
// delta than to block the LLM stream).
func (s *wsSession) send(f Frame) {
	select {
	case s.writeCh <- f:
	default:
		s.log.Warn().Str("type", f.Type).Msg("ws write queue full, dropping frame")
	}
}

// close shuts down the session. Idempotent.
func (s *wsSession) close() {
	s.closeOnce.Do(func() {
		close(s.writeCh)
		_ = s.conn.Close()
	})
}

// writePump drains writeCh and writes each frame to the conn. It
// also sends a periodic ping so the browser knows the connection
// is alive and stale tabs detect a dead server.
func (s *wsSession) writePump(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case f, ok := <-s.writeCh:
			if !ok {
				return
			}
			if err := s.conn.WriteJSON(f); err != nil {
				s.log.Debug().Err(err).Msg("ws write failed")
				return
			}
		case <-ticker.C:
			if err := s.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// startTurn attempts to mark an LLM turn as in-flight. Returns false
// when one is already running. endTurn releases the lock. The
// caller-supplied id is stamped onto every outgoing frame that
// belongs to the turn, so the client can group them and avoid
// cross-turn replacement.
func (s *wsSession) startTurn(id string) bool {
	s.turnMtx.Lock()
	defer s.turnMtx.Unlock()
	if s.turnInFlight {
		return false
	}
	s.turnInFlight = true
	s.currentTurnID = id
	s.bufMtx.Lock()
	s.buf.Reset()
	s.jsonMode = false
	s.textSeen = false
	s.bufMtx.Unlock()
	s.tokensMtx.Lock()
	s.tokens = TokenUsage{}
	s.tokensMtx.Unlock()

	return true
}

func (s *wsSession) endTurn() {
	s.turnMtx.Lock()
	s.turnInFlight = false
	s.currentTurnID = ""
	s.turnMtx.Unlock()
}

// currentID returns the active turn id. Empty when no turn is in
// flight. Used by the server to stamp frames before sending.
func (s *wsSession) currentID() string {
	s.turnMtx.Lock()
	defer s.turnMtx.Unlock()

	return s.currentTurnID
}

// recordTokens stores the latest per-round usage the GM reported.
// The OnTokens callback fires once per LLM round (the GM streams
// several rounds in tool-call loops), so we keep the most recent
// values for prompt and completion and accumulate into total.
func (s *wsSession) recordTokens(prompt, completion, total int, source string) {
	s.tokensMtx.Lock()
	defer s.tokensMtx.Unlock()
	// Last-round prompt is the most recent number (counts grow as
	// the context builds); completion is the running total of the
	// final round, so we take the larger of the two to avoid
	// regressions.
	if prompt > s.tokens.PromptTokens {
		s.tokens.PromptTokens = prompt
	}
	if completion > s.tokens.CompletionTokens {
		s.tokens.CompletionTokens = completion
	}
	if total > s.tokens.TotalTokens {
		s.tokens.TotalTokens = total
	}
	if source != "" {
		s.tokens.Source = source
	}
}

// currentTokens returns the accumulated usage snapshot for the
// current turn. The server reads this when emitting the final
// FrameMessage so the assistant bubble can carry a token footer.
func (s *wsSession) currentTokens() TokenUsage {
	s.tokensMtx.Lock()
	defer s.tokensMtx.Unlock()

	return s.tokens
}

// accumulateDelta appends a text delta to the assistant buffer and
// tracks JSON-mode transition. Called by the server's OnDelta
// callback.
func (s *wsSession) accumulateDelta(delta string) {
	s.bufMtx.Lock()
	s.textSeen = true
	s.buf.WriteString(delta)
	if !s.jsonMode && structured.LooksLikeJSON(s.buf.String()) {
		s.jsonMode = true
	}
	s.bufMtx.Unlock()
}

// currentText returns the full accumulated assistant buffer.
func (s *wsSession) currentText() string {
	s.bufMtx.Lock()
	defer s.bufMtx.Unlock()

	return s.buf.String()
}

// isJSONMode reports whether the current turn switched to silent
// JSON accumulation.
func (s *wsSession) isJSONMode() bool {
	s.bufMtx.Lock()
	defer s.bufMtx.Unlock()

	return s.jsonMode
}

// textSeen reports whether any content delta has arrived for the
// current turn.
func (s *wsSession) hasText() bool {
	s.bufMtx.Lock()
	defer s.bufMtx.Unlock()

	return s.textSeen
}

// finalText renders the accumulated assistant buffer for the
// just-completed turn. Called by the server after the LLM stream
// finishes. Strips thinking tags and applies the JSON-mode render.
func (s *wsSession) finalText() string {
	s.bufMtx.Lock()
	raw := s.buf.String()
	jsonMode := s.jsonMode
	s.bufMtx.Unlock()
	raw = structured.StripThinkingTags(raw)
	if jsonMode {
		n, err := structured.Parse(raw)
		if err == nil {
			return n.Render()
		}
	}

	return raw
}
