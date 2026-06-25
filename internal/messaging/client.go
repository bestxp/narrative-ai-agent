// Package messaging defines the transport-agnostic surface that any
// chat client (Telegram, Discord, Web, ...) must satisfy. The lazy-
// universe bot targets any number of clients simultaneously; the GM
// usecase consumes a single Client and is unaware of the underlying
// platform.
package messaging

import (
	"context"
	"time"
)

// HealthState is the lifecycle state a Client reports back to
// the health server. Strings mirror the values exposed by the
// health package so that the same enum flows through the wire.
type HealthState string

const (
	StateUnknown   HealthState = "unknown"
	StateStarting  HealthState = "starting"
	StateConnected HealthState = "connected"
	StateReconnect HealthState = "reconnecting"
	StateStopped   HealthState = "stopped"
)

// HealthReport is the snapshot a Client exposes to the health
// server. Name is a transport identifier ("telegram", "vk"); State
// is one of the HealthState constants; StartedAt is when the
// transport first reached StateConnected (zero-value time on
// transports that have not yet connected); Message is a
// free-form human-readable detail (last error, last reconnect
// delay, etc.) — must not contain secrets.
type HealthReport struct {
	Name      string
	State     HealthState
	StartedAt time.Time
	Message   string
}

// Sender is a minimal abstraction over "who is allowed to talk to
// the bot" and "where do we send replies". Each transport maps the
// native user id to a string SenderID so the GM can reason about
// players uniformly (e.g. persist last-seen timestamps per sender).
type Sender struct {
	ID   string
	Name string
}

// IncomingMessage is the platform-agnostic representation of a user
// message arriving at the bot.
type IncomingMessage struct {
	Sender Sender
	ChatID string
	Text   string
	// MessageID is the platform-native id of the message. Used to
	// thread bot replies back to the originating message on
	// transports that support it (Telegram: reply_to_message_id).
	// Zero on platforms that don't carry an id.
	MessageID int
	Command   string
	Args      []string
}

// OutgoingMessage is what the bot wants to deliver to a chat. Edit
// replaces the original message (used for streaming), Send posts a
// new one.
type OutgoingMessage struct {
	ChatID    string
	Text      string
	ParseMode string
	// ReplyToMessageID, when > 0, threads the outgoing message as
	// a reply to the originating user message. Set to 0 for
	// standalone messages (auto-save notifications, compaction
	// notices, error pop-ups) that should appear as their own
	// bubbles rather than riding the user's message thread.
	ReplyToMessageID int
}

// StreamSession is opened by Edit and yields the chat id the bot can
// call Edit() against. Closing it is implementation specific — the
// Close method flushes the last buffer and finalises the message.
type StreamSession interface {
	// Append replaces the visible text with the current buffer.
	Append(ctx context.Context, text string) error
	// Final replaces the visible text one last time (e.g. with the
	// full response) and closes the stream. The session is unusable
	// after Final.
	Final(ctx context.Context, text string) error
}

// BotCommand is the transport-agnostic description of a single
// command hint. The Description is what the user sees in the
// platform's native command picker (Telegram: the menu that
// pops up when "/" is typed in the chat input). Keeping it
// short (≤256 chars per Telegram) is the caller's job; the
// transport is free to truncate or reject longer strings.
type BotCommand struct {
	Command     string
	Description string
}

// Client is the abstraction every transport implements. It is
// deliberately small: receive a message, send a reply, stream a
// reply, and ask whether a sender is on the allow list.
type Client interface {
	// Name returns a short identifier used in logs ("telegram",
	// "discord", "web").
	Name() string

	// Run blocks until ctx is cancelled or the underlying transport
	// exits. It is safe to call Run for multiple clients in
	// goroutines — the package owner is responsible for lifecycle.
	Run(ctx context.Context) error

	// Health returns the current state of the transport. It is
	// safe to call from any goroutine at any time and must not
	// block. Health probes poll this on a 1-5s cadence; clients
	// should report the last known state without re-validating.
	Health() HealthReport

	// Send posts a new message to the chat. The transport is free
	// to split very long messages.
	Send(ctx context.Context, msg OutgoingMessage) error

	// StartStream opens a streaming session to the chat. The first
	// call posts an empty placeholder; subsequent Append calls edit
	// it. Final must be called exactly once. replyToMessageID,
	// when > 0, threads the placeholder as a reply to the user's
	// message on transports that support it (Telegram). Pass 0
	// for transports that don't carry an originating id or when
	// the operator disabled reply threading.
	StartStream(ctx context.Context, chatID string, replyToMessageID int) (StreamSession, error)

	// IsAllowed reports whether the sender id is permitted to drive
	// the GM (per-messenger allow list lives in config).
	IsAllowed(senderID string) bool

	// SetCommands registers the bot's command hints with the
	// transport. Telegram translates this to the native menu
	// shown when the user types "/". Transports that don't
	// support command hints may implement this as a no-op.
	SetCommands(ctx context.Context, cmds []BotCommand) error
}

// MultiClient fans a single IncomingMessage channel out to many
// transports and consolidates the resulting streams. It is the
// "hub" wiring layer used by main.go.
type MultiClient struct {
	clients []Client
}

// NewMultiClient composes several clients. The order is preserved
// for log clarity; behaviour is otherwise identical.
func NewMultiClient(clients ...Client) *MultiClient {
	return &MultiClient{clients: clients}
}

// Run starts every client in its own goroutine and blocks until
// either ctx is cancelled or all clients exit.
func (m *MultiClient) Run(ctx context.Context) error {
	if len(m.clients) == 0 {
		<-ctx.Done()
		return ctx.Err()
	}
	errCh := make(chan error, len(m.clients))
	for _, c := range m.clients {
		go func() {
			errCh <- c.Run(ctx)
		}()
	}
	for range m.clients {
		if err := <-errCh; err != nil {
			return err
		}
	}
	return nil
}

// All returns the underlying clients. Useful for the GM to pick a
// specific transport (e.g. reply via the same channel the message
// arrived on).
func (m *MultiClient) All() []Client {
	return m.clients
}
