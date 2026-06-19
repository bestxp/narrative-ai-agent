package wschat

import (
	"context"
	"strings"

	"github.com/rs/zerolog"

	"github.com/bestxp/narrative-ai-agent/internal/config"
	"github.com/bestxp/narrative-ai-agent/internal/dispatcher"
	"github.com/bestxp/narrative-ai-agent/internal/messaging"
)

// Client is the messaging.Client implementation for the browser
// WebSocket chat. It owns one Server (one HTTP listener, one
// logical chat_id) and feeds incoming messages into the dispatcher.
//
// Unlike Telegram / VK, inbound messages do not arrive via a
// long-poll loop — they arrive over the WebSocket, where the server
// calls the dispatcher directly. The Recv channel is therefore
// unused; the Client still implements the receiver interface with
// a closed channel so main.go's range over it exits immediately.
type Client struct {
	cfg    config.WSChatConfig
	srv    *Server
	log    zerolog.Logger
	recvCh chan messaging.IncomingMessage
}

// New constructs a wschat Client. The dispatcher is used to drive
// command / freeform / edit / resend flows. commands is the initial
// command hint list (the dispatcher's Commands() output).
func New(cfg config.WSChatConfig, disp *dispatcher.Dispatcher, commands []messaging.BotCommand, log zerolog.Logger) (*Client, error) {
	auth := AuthConfig{DevToken: cfg.DevToken, AllowedTokens: cfg.AllowedTokens}
	srv := NewServer(cfg.ListenAddr, cfg.ChatID, auth, disp, commands, log)
	recvCh := make(chan messaging.IncomingMessage) // never written; see Recv.
	close(recvCh)
	return &Client{
		cfg:    cfg,
		srv:    srv,
		log:    log,
		recvCh: recvCh,
	}, nil
}

// Name returns the transport identifier.
func (c *Client) Name() string { return "wschat" }

// Run blocks until ctx is cancelled, serving the HTTP+WS server.
func (c *Client) Run(ctx context.Context) error {
	return c.srv.Run(ctx)
}

// Health reports the current transport state.
func (c *Client) Health() messaging.HealthReport {
	return c.srv.Health()
}

// Send posts a new message to the active WebSocket session. Used by
// the auto-save notify path in main.go. When no session is attached
// the message is dropped silently (dev: the tab is closed).
func (c *Client) Send(ctx context.Context, msg messaging.OutgoingMessage) error {
	c.srv.sendMessage("assistant", msg.Text, "")
	return nil
}

// StartStream opens a streaming session. The wschat transport
// handles streaming natively over the WebSocket (delta frames), so
// the session here just forwards Append/Final to the server's
// sendDelta / sendMessage. replyToMessageID is ignored — the
// browser UI threads by ordering, not by reply id.
func (c *Client) StartStream(ctx context.Context, chatID string, replyToMessageID int) (messaging.StreamSession, error) {
	return &streamSession{srv: c.srv}, nil
}

// IsAllowed reports whether the sender id is permitted. The wschat
// transport authorises on the WebSocket upgrade, not per-message,
// so this always returns true — once a session is attached the
// operator is trusted.
func (c *Client) IsAllowed(senderID string) bool { return true }

// SetCommands replaces the command hint list served by
// /api/commands and pushed to the active session on demand.
func (c *Client) SetCommands(ctx context.Context, cmds []messaging.BotCommand) error {
	c.srv.SetCommands(cmds)
	return nil
}

// Recv returns the incoming-message channel. The wschat transport
// dispatches directly inside the WebSocket read loop, so this
// channel is always closed (main.go's range exits immediately and
// the dispatcher is driven from the server instead).
func (c *Client) Recv() <-chan messaging.IncomingMessage {
	return c.recvCh
}

// streamSession is the messaging.StreamSession returned by
// StartStream. It is a thin adapter: Append pushes a delta frame,
// Final pushes a delta then a final message frame.
type streamSession struct {
	srv   *Server
	final bool
}

func (s *streamSession) Append(ctx context.Context, text string) error {
	if s.final {
		return nil
	}
	s.srv.sendDelta(text)
	return nil
}

func (s *streamSession) Final(ctx context.Context, text string) error {
	if s.final {
		return nil
	}
	s.final = true
	s.srv.sendMessage("assistant", text, "")
	return nil
}

// joinArgs renders the args slice as a space-joined string with a
// leading space, or empty when there are no args. Used to display a
// command invocation as "/me arg1 arg2".
func joinArgs(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return " " + strings.Join(args, " ")
}
