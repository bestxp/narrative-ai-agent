package wschat

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/bestxp/narrative-ai-agent/internal/adapter/llm"
	"github.com/bestxp/narrative-ai-agent/internal/dispatcher"
	"github.com/bestxp/narrative-ai-agent/internal/messaging"
	"github.com/bestxp/narrative-ai-agent/internal/usecase"
	"github.com/gorilla/websocket"
	"github.com/rs/zerolog"
)

// Server is the HTTP+WebSocket server that backs the wschat
// transport. It serves the embedded React app on "/", the command
// list on "/api/commands", and the WebSocket endpoint on "/ws".
// One Server serves one logical chat (the dev chat_id from config).
type Server struct {
	addr     string
	chatID   string
	auth     AuthConfig
	commands []messaging.BotCommand
	disp     *dispatcher.Dispatcher
	log      zerolog.Logger
	upgrader websocket.Upgrader
	httpSrv  *http.Server

	// session is the single active WebSocket connection. In dev the
	// chat has exactly one operator; a second connection replaces the
	// first (the old one is closed so the browser does not hold two
	// sockets when the tab is reloaded). Production multi-user
	// routing is a future concern — the auth hook is already here.
	sessionMtx sync.Mutex
	session    *wsSession

	// state tracks the last user message text so edit_last and
	// resend_last can operate without round-tripping through the
	// GM conversation. The GM is the source of truth; this is just
	// a convenience cache so the server can answer edit/resend
	// requests before the GM call.
	lastUserText string

	// health is the state reported to the messaging.Client.Health
	// method. connected becomes true once the HTTP server is bound
	// and stays true until shutdown.
	healthMtx sync.Mutex
	health    messaging.HealthReport
}

// NewServer constructs a Server bound to addr, serving the given
// chatID. The dispatcher is used to drive freeform / command /
// edit / resend flows; commands is the initial command hint list
// (refreshable via SetCommands).
func NewServer(addr, chatID string, auth AuthConfig, disp *dispatcher.Dispatcher, commands []messaging.BotCommand, log zerolog.Logger) *Server {
	s := &Server{
		addr:     addr,
		chatID:   chatID,
		auth:     auth,
		commands: commands,
		disp:     disp,
		log:      log.With().Str("component", "wschat").Logger(),
		upgrader: websocket.Upgrader{
			// Dev server: allow any origin. The browser connects
			// from the same host:port so this is not a hole in
			// practice; tightening per-origin is a production
			// concern.
			CheckOrigin: func(_ *http.Request) bool { return true },
		},
	}
	s.health = messaging.HealthReport{
		Name:  "wschat",
		State: messaging.StateStarting,
	}

	return s
}

// Run binds the HTTP server and blocks until ctx is cancelled. The
// server reports StateConnected once it is listening and
// StateStopped on exit.
func (s *Server) Run(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/commands", s.handleCommandsAPI)
	mux.HandleFunc("/ws", s.handleWS)

	s.httpSrv = &http.Server{
		Addr:              s.addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	listenErr := make(chan error, 1)

	go func() {
		s.log.Info().Str("addr", s.addr).Msg("wschat server listening")
		err := s.httpSrv.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			listenErr <- err
			return
		}

		listenErr <- nil
	}()

	s.setHealth(messaging.HealthReport{
		Name:      "wschat",
		State:     messaging.StateConnected,
		StartedAt: time.Now(),
		Message:   "listening on " + s.addr,
	})

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		_ = s.httpSrv.Shutdown(shutCtx)
		s.closeSession()
		s.setHealth(messaging.HealthReport{Name: "wschat", State: messaging.StateStopped})

		return nil
	case err := <-listenErr:
		s.setHealth(messaging.HealthReport{Name: "wschat", State: messaging.StateStopped, Message: "bind failed"})
		return err
	}
}

// Health returns the last known transport state. Safe to call from
// any goroutine.
func (s *Server) Health() messaging.HealthReport {
	s.healthMtx.Lock()
	defer s.healthMtx.Unlock()

	return s.health
}

func (s *Server) setHealth(r messaging.HealthReport) {
	s.healthMtx.Lock()
	s.health = r
	s.healthMtx.Unlock()
}

// SetCommands replaces the command hint list. Called by the
// messaging.Client SetCommands method.
func (s *Server) SetCommands(cmds []messaging.BotCommand) {
	s.sessionMtx.Lock()
	s.commands = cmds
	s.sessionMtx.Unlock()
}

// handleIndex serves the embedded React app. The static handler is
// built at package init via embedStaticHandler (see web.go).
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	serveStatic(w, r)
}

// handleCommandsAPI returns the current command hint list as JSON.
// Auth-protected so the dev_token is not leaked to anyone scanning
// the port.
func (s *Server) handleCommandsAPI(w http.ResponseWriter, r *http.Request) {
	if !requireAuth(w, r, s.auth) {
		return
	}
	s.sessionMtx.Lock()
	cmds := s.commands
	s.sessionMtx.Unlock()

	out := CommandListPayload{Commands: make([]CommandDesc, 0, len(cmds))}
	for _, c := range cmds {
		out.Commands = append(out.Commands, CommandDesc{Command: c.Command, Description: c.Description})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out) //nolint:errchkjson // HTTP response stream is already started; encode errors are unobservable to the client
}

// handleWS upgrades the HTTP request to a WebSocket and runs the
// read/write loops until the connection closes or the server shuts
// down.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	if !requireAuth(w, r, s.auth) {
		return
	}
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.log.Warn().Err(err).Msg("ws upgrade failed")
		return
	}
	sess := newSession(conn, s.chatID, s.log)

	// Replace any existing session: close it so a tab reload does
	// not leave a dangling socket. The old session's write loop
	// will observe the closed conn and exit.
	s.sessionMtx.Lock()
	old := s.session
	s.session = sess
	s.sessionMtx.Unlock()
	if old != nil {
		old.close()
	}

	s.log.Info().Str("chat", s.chatID).Msg("ws session attached")
	defer s.log.Info().Str("chat", s.chatID).Msg("ws session detached")

	sess.run(r.Context(), s)
}

// closeSession closes the active session if any. Called on shutdown.
func (s *Server) closeSession() {
	s.sessionMtx.Lock()
	sess := s.session
	s.session = nil
	s.sessionMtx.Unlock()
	if sess != nil {
		sess.close()
	}
}

// activeSession returns the current session under the lock. Returns
// nil when no session is attached.
func (s *Server) activeSession() *wsSession {
	s.sessionMtx.Lock()
	defer s.sessionMtx.Unlock()

	return s.session
}

// sendFrame writes a frame to the active session. Drops silently
// when no session is attached (dev: the operator closed the tab).
func (s *Server) sendFrame(f Frame) {
	if sess := s.activeSession(); sess != nil {
		// Stamp the current turn id on the frame when one is in
		// flight and the frame has no id yet. This groups delta
		// and status frames into the turn that produced them, so
		// the client can update the right placeholder even after
		// the user has fired a second turn.
		if f.ID == "" {
			if id := sess.currentID(); id != "" {
				f.ID = id
			}
		}
		sess.send(f)
	}
}

// sendMessage pushes a FrameMessage. tokens, when non-zero, is
// attached to the payload so the client can show a token footer.
func (s *Server) sendMessage(role, text, command string) {
	mp := MessagePayload{Role: role, Text: text, Command: command}
	if role == "assistant" {
		if sess := s.activeSession(); sess != nil {
			if tok := sess.currentTokens(); tok.TotalTokens > 0 || tok.Source != "" {
				mp.Tokens = &tok
			}
		}
	}
	payload, _ := json.Marshal(mp) //nolint:errchkjson // wire frame for fixed-shape struct; marshal cannot fail
	s.sendFrame(Frame{Type: FrameMessage, Payload: payload})
}

// sendDelta pushes a FrameDelta with the full current assistant text.
func (s *Server) sendDelta(text string) {
	payload, _ := json.Marshal(DeltaPayload{Text: text}) //nolint:errchkjson // wire frame for fixed-shape struct; marshal cannot fail
	s.sendFrame(Frame{Type: FrameDelta, Payload: payload})
}

// sendStatus pushes a FrameStatus.
func (s *Server) sendStatus(phase string, details map[string]any) {
	payload, _ := json.Marshal(StatusPayload{Phase: phase, Details: details}) //nolint:errchkjson // wire frame for fixed-shape struct; marshal cannot fail
	s.sendFrame(Frame{Type: FrameStatus, Payload: payload})
}

// sendError pushes a FrameError with an optional correlation id.
func (s *Server) sendError(id, code, msg string) {
	payload, _ := json.Marshal(ErrorPayload{Code: code, Message: msg}) //nolint:errchkjson // wire frame for fixed-shape struct; marshal cannot fail
	s.sendFrame(Frame{Type: FrameError, ID: id, Payload: payload})
}

// sendAck pushes a FrameAck with an optional correlation id.
// The payload's OK field is reserved for future NACK frames;
// today every ack is positive and carries no message so we
// hard-code both here.
func (s *Server) sendAck(id string) {
	payload, _ := json.Marshal(AckPayload{OK: true}) //nolint:errchkjson // wire frame for fixed-shape struct; marshal cannot fail
	s.sendFrame(Frame{Type: FrameAck, ID: id, Payload: payload})
}

// handleClientFrame dispatches one client→server frame. It is
// called from the session's read loop. The session tracks its own
// in-flight LLM call so edit_last / resend_last arriving while the
// model is streaming are queued or rejected cleanly.
func (s *Server) handleClientFrame(ctx context.Context, sess *wsSession, f Frame) {
	switch f.Type {
	case FrameSend:
		var p SendPayload
		if err := json.Unmarshal(f.Payload, &p); err != nil {
			s.sendError(f.ID, "bad_payload", err.Error())
			return
		}
		s.handleSend(ctx, sess, f.ID, p)
	case FrameEditLast:
		var p EditPayload
		if err := json.Unmarshal(f.Payload, &p); err != nil {
			s.sendError(f.ID, "bad_payload", err.Error())
			return
		}
		s.handleEditLast(ctx, sess, f.ID, p.NewText)
	case FrameResendLast:
		s.handleResendLast(ctx, sess, f.ID)
	case FrameCommandListRequest:
		s.sessionMtx.Lock()
		cmds := s.commands
		s.sessionMtx.Unlock()
		out := CommandListPayload{Commands: make([]CommandDesc, 0, len(cmds))}
		for _, c := range cmds {
			out.Commands = append(out.Commands, CommandDesc{Command: c.Command, Description: c.Description})
		}
		payload, _ := json.Marshal(out) //nolint:errchkjson // wire frame for fixed-shape struct; marshal cannot fail
		sess.send(Frame{Type: FrameCommandList, ID: f.ID, Payload: payload})
	default:
		// Unknown type: ignore (forward-compat).
		s.log.Debug().Str("type", f.Type).Msg("unknown frame ignored")
	}
}

// handleSend processes a FrameSend: either a slash command or a
// freeform LLM turn. The reply streams back through the session.
// The client supplies a fresh id per frame; we use it as the turn
// id so all reply frames (ack, delta, status, message) carry it
// and the client can group them. The user message itself is not
// pushed optimistically by the client — the server echoes it back
// as a FrameMessage so there is exactly one bubble per turn.
func (s *Server) handleSend(ctx context.Context, sess *wsSession, id string, p SendPayload) {
	// Serialize: only one in-flight LLM turn per chat. The lock is
	// per-server (one chat_id), so a second send while the first is
	// streaming is rejected with a short error.
	if !sess.startTurn(id) {
		s.sendError(id, "busy", "another turn is in progress")
		return
	}
	defer sess.endTurn()

	chatID := s.chatID
	if p.Command != "" {
		// Echo the user message back so the UI shows the command
		// the operator just typed. The id stamped on this FrameMessage
		// matches the turn id so the client groups the user bubble
		// with the assistant reply that follows.
		s.sendMessage("user", "/"+p.Command+joinArgs(p.Args), p.Command)
		msg := messaging.IncomingMessage{
			Sender:  messaging.Sender{ID: "dev", Name: "dev"},
			ChatID:  chatID,
			Text:    p.Text,
			Command: p.Command,
			Args:    p.Args,
		}
		reply, err := s.disp.Handle(ctx, msg)
		if err != nil {
			s.sendError(id, "command_error", err.Error())
			return
		}
		s.sendAck(id)
		if reply != "" {
			s.sendMessage("assistant", reply, "")
		}

		return
	}

	// Freeform user message.
	if p.Text == "" {
		s.sendError(id, "empty", "text or command required")
		return
	}
	s.sendMessage("user", p.Text, "")
	s.lastUserText = p.Text
	s.sendAck(id)

	cb := s.callbacks(sess)
	msg := messaging.IncomingMessage{
		Sender: messaging.Sender{ID: "dev", Name: "dev"},
		ChatID: chatID,
		Text:   p.Text,
	}
	if err := s.disp.HandleStream(ctx, msg, cb); err != nil {
		s.sendError(id, "llm_error", err.Error())
		return
	}
	final := sess.finalText()
	s.sendMessage("assistant", final, "")
}

// handleEditLast replaces the last user message with newText and
// regenerates the LLM answer. The user message is NOT re-echoed:
// the client already has it (the operator is editing an existing
// bubble) and already updated the text optimistically. The edit
// request's id is the turn id used for the streaming assistant
// and the final assistant message, so the client can group the
// regenerated reply under the edited user bubble without
// producing a duplicate.
func (s *Server) handleEditLast(ctx context.Context, sess *wsSession, id, newText string) {
	if !sess.startTurn(id) {
		s.sendError(id, "busy", "another turn is in progress")
		return
	}
	defer sess.endTurn()

	if newText == "" {
		s.sendError(id, "empty", "new_text required")
		return
	}
	s.lastUserText = newText
	s.sendAck(id)

	cb := s.callbacks(sess)
	if err := s.disp.EditLast(ctx, s.chatID, newText, cb); err != nil {
		if errors.Is(err, usecase.ErrNoLastUserTurn) {
			s.sendError(id, "no_last_user", err.Error())

			return
		}
		s.sendError(id, "llm_error", err.Error())

		return
	}
	final := sess.finalText()
	s.sendMessage("assistant", final, "")
}

// handleResendLast regenerates the LLM answer for the last user
// message, discarding the previous assistant reply. The user
// message is NOT re-echoed — the client already has it from the
// original send / edit, and it already deleted the prior assistant
// bubble so the streaming placeholder lands in the right slot.
// The resend's id is the turn id used for the streaming assistant
// and the final assistant message.
func (s *Server) handleResendLast(ctx context.Context, sess *wsSession, id string) {
	if !sess.startTurn(id) {
		s.sendError(id, "busy", "another turn is in progress")
		return
	}
	defer sess.endTurn()

	s.sendAck(id)
	cb := s.callbacks(sess)
	if err := s.disp.ResendLast(ctx, s.chatID, cb); err != nil {
		if errors.Is(err, usecase.ErrNoLastUserTurn) {
			s.sendError(id, "no_last_user", err.Error())

			return
		}
		s.sendError(id, "llm_error", err.Error())

		return
	}
	final := sess.finalText()
	s.sendMessage("assistant", final, "")
}

// callbacks builds the usecase.Callbacks that drive the streaming
// UI. The status and delta frames go to the active session; the
// assistant buffer is tracked inside the session so finalText
// can produce the final rendered body.
func (s *Server) callbacks(sess *wsSession) usecase.Callbacks {
	return usecase.Callbacks{
		OnDelta: func(delta string) error {
			sess.accumulateDelta(delta)
			if sess.isJSONMode() {
				return nil
			}
			// Replace the assistant bubble's visible text with the
			// full current buffer (replace semantics, not append).
			s.sendDelta(sess.currentText())

			return nil
		},
		OnStatus: func(phase string, details map[string]any) {
			if sess.hasText() {
				return
			}
			s.sendStatus(phase, details)
		},
		OnTokens: func(u llm.Usage) {
			sess.recordTokens(u.PromptTokens, u.CompletionTokens, u.TotalTokens, "usage")
		},
		OnCompaction: func(r usecase.CompactionResult) {
			s.sendMessage("assistant", usecase.DescribeCompaction(r, "narrative", false), "")
		},
	}
}
