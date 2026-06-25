package telegram

// Package telegram implements the messaging.Client interface for the
// Telegram Bot API. It uses long-polling; switching to webhooks is a
// future optimisation, not a refactor.
//
// Markdown conversion is delegated to eekstunt/telegramify-markdown-go,
// which produces Telegram MessageEntity objects (the API's native
// formatting format) from a Markdown source. Our wrapper walks those
// entities and emits Telegram-flavoured HTML so we can drive both
// sendMessage and editMessageText with parse_mode=HTML.

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bestxp/narrative-ai-agent/internal/messaging"
	tg "github.com/go-telegram-bot-api/telegram-bot-api"
	"github.com/rs/zerolog"
)

// Config holds the per-transport settings. main.go is responsible
// for reading them from config.MessagingConfig.Telegram.
type Config struct {
	Token          string
	PollingTimeout int
	ParseMode      string
	AllowedUserIDs []int
}

// Client is a Telegram implementation of messaging.Client.
type Client struct {
	cfg           Config
	api           *tg.BotAPI
	log           zerolog.Logger
	recv          chan messaging.IncomingMessage
	isOpen        bool
	streamsMu     sync.Mutex
	activeStreams map[string]int

	// healthMu guards the HealthReport snapshot. Health() is
	// called from the HTTP probe goroutine on a 1-5s cadence;
	// the Run() goroutine mutates these fields on connect /
	// reconnect / shutdown. RWMutex is fine because the probe
	// takes a snapshot.
	healthMu        sync.RWMutex
	healthState     messaging.HealthState
	healthStartedAt time.Time
	healthMessage   string
}

// New authenticates with BotFather and prepares the client. The
// returned client must be closed via Close() if Run is not used.
func New(cfg Config, log zerolog.Logger) (*Client, error) {
	if cfg.Token == "" {
		return nil, errors.New("telegram: empty token")
	}
	api, err := tg.NewBotAPI(cfg.Token)
	if err != nil {
		return nil, fmt.Errorf("telegram: auth: %w", err)
	}
	if cfg.PollingTimeout == 0 {
		cfg.PollingTimeout = 60
	}

	return &Client{
		cfg:         cfg,
		api:         api,
		log:         log.With().Str("transport", "telegram").Logger(),
		recv:        make(chan messaging.IncomingMessage, 64),
		healthState: messaging.StateStarting,
	}, nil
}

// Name implements messaging.Client.
func (c *Client) Name() string { return "telegram" }

// Recv returns the channel of incoming messages. Multiple goroutines
// may read from it (each delivery is to one reader).
func (c *Client) Recv() <-chan messaging.IncomingMessage {
	return c.recv
}

// IsAllowed implements messaging.Client.
func (c *Client) IsAllowed(senderID string) bool {
	id, err := strconv.Atoi(senderID)
	if err != nil {
		return false
	}

	return slices.Contains(c.cfg.AllowedUserIDs, id)
}

// SetCommands implements messaging.Client. It registers the
// bot's command list with the Telegram Bot API so the client
// shows a native menu when the user types "/" in the chat input.
// The scope is "default" — i.e. the commands are visible to
// every user who can talk to the bot. Per-user scopes
// (admin-only commands) are a future extension.
//
// The go-telegram-bot-api v4.6.4 wrapper does not expose
// setMyCommands as a typed helper, so we hit the raw
// /setMyCommands endpoint via MakeRequest. The wire format
// is a flat array of {command, description} objects — the
// same shape the BotFather UI uses.
func (c *Client) SetCommands(_ context.Context, cmds []messaging.BotCommand) error {
	params := make(map[string]string)
	params["commands"] = encodeCommandsJSON(cmds)
	_, err := c.api.MakeRequest("setMyCommands", asURLValues(params))
	if err != nil {
		c.log.Warn().Err(err).Int("count", len(cmds)).Msg("telegram: setMyCommands failed")
		return fmt.Errorf("set_commands: MakeRequest failed: %w", err)
	}
	c.log.Info().Int("count", len(cmds)).Msg("telegram: setMyCommands ok")

	return nil
}

// encodeCommandsJSON serialises a command list the way the
// Telegram API expects: a single JSON array string passed as
// the "commands" query parameter. We hand-build the JSON
// rather than calling json.Marshal so we don't pull in any
// allocation overhead — this is a startup-time call only.
func encodeCommandsJSON(cmds []messaging.BotCommand) string {
	if len(cmds) == 0 {
		return "[]"
	}
	var b strings.Builder
	b.WriteString("[")
	for i, c := range cmds {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"command":`)
		b.WriteString(jsonString(c.Command))
		b.WriteString(`,"description":`)
		b.WriteString(jsonString(c.Description))
		b.WriteString("}")
	}
	b.WriteString("]")

	return b.String()
}

// jsonString returns a JSON string literal for s. The
// conversion is intentionally minimal — Telegram does not
// accept special characters in command names (a-z, 0-9, _),
// so the only characters that need escaping in descriptions
// are " and \ plus the basic control characters.
func jsonString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				continue
			}
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')

	return b.String()
}

// asURLValues is a tiny adapter from map[string]string to
// url.Values. We can't import net/url here without bloating
// the import block; the parameter shape is trivial.
func asURLValues(m map[string]string) url.Values {
	v := make(url.Values, len(m))
	for k, val := range m {
		v.Set(k, val)
	}

	return v
}

// Run implements messaging.Client. It blocks until ctx is cancelled
// or the polling channel closes.
func (c *Client) Run(ctx context.Context) error {
	u := tg.NewUpdate(0)
	u.Timeout = c.cfg.PollingTimeout
	updates, err := c.api.GetUpdatesChan(u)
	if err != nil {
		c.setHealth(messaging.StateReconnect, "", err.Error())
		return fmt.Errorf("telegram: get updates: %w", err)
	}
	c.setHealth(messaging.StateConnected, c.api.Self.UserName, "")
	c.log.Info().Str("username", c.api.Self.UserName).Msg("bot started")

	// Typing ticker: refreshes "typing..." for chats with active streams.
	go c.typingLoop(ctx)

	for {
		select {
		case <-ctx.Done():
			c.setHealth(messaging.StateStopped, "", "ctx cancelled")
			c.log.Info().Msg("shutdown")

			return nil
		case upd, ok := <-updates:
			if !ok {
				c.setHealth(messaging.StateReconnect, "", "updates channel closed")

				return errors.New("telegram: updates channel closed")
			}
			if upd.Message == nil {
				continue
			}
			if !c.IsAllowed(strconv.Itoa(upd.Message.From.ID)) {
				c.log.Warn().Int("user_id", upd.Message.From.ID).Msg("rejected user")
				continue
			}
			text := strings.TrimSpace(upd.Message.Text)
			msg := messaging.IncomingMessage{
				Sender: messaging.Sender{
					ID:   strconv.Itoa(upd.Message.From.ID),
					Name: upd.Message.From.UserName,
				},
				ChatID:    strconv.FormatInt(upd.Message.Chat.ID, 10),
				Text:      text,
				MessageID: upd.Message.MessageID,
			}
			if strings.HasPrefix(text, "/") {
				parts := strings.Fields(text)
				msg.Command = strings.TrimPrefix(parts[0], "/")
				if len(parts) > 1 {
					msg.Args = parts[1:]
				}
			}

			c.recv <- msg
		}
	}
}

// Send implements messaging.Client.
func (c *Client) Send(_ context.Context, msg messaging.OutgoingMessage) error {
	wire := c.formatText(msg.Text, msg.ParseMode)
	// Telegram caps each message at 4096 characters. Long
	// freeform replies (e.g. /me dump of a 7k-character
	// character) have to be split; the first chunk is the
	// "main" reply, the rest are continuations sent as
	// fresh messages in the same chat.
	chunks := splitForTelegram(wire)
	for i, chunk := range chunks {
		m := tg.NewMessage(parseChatID(msg.ChatID), chunk)
		if msg.ParseMode != "" {
			m.ParseMode = msg.ParseMode
		} else if c.cfg.ParseMode != "" {
			m.ParseMode = c.cfg.ParseMode
		}
		// Only the first chunk carries the reply_to
		// threading — continuations are not replies to the
		// user's message, they are follow-ups to the
		// bot's own preceding message.
		if i == 0 && msg.ReplyToMessageID > 0 {
			m.ReplyToMessageID = msg.ReplyToMessageID
		}
		if _, err := c.api.Send(m); err != nil {
			c.log.Error().Err(err).Str("chat", msg.ChatID).Int("reply_to", msg.ReplyToMessageID).Int("text_len", len(chunk)).Int("chunk", i).Msg("telegram: send failed")
			return fmt.Errorf("wrap: Send failed: %w", err)
		}
	}
	c.log.Debug().Str("chat", msg.ChatID).Int("reply_to", msg.ReplyToMessageID).Int("text_len", len(msg.Text)).Int("chunks", len(chunks)).Msg("send ok")

	return nil
}

// StartStream implements messaging.Client.
//
//nolint:ireturn // interface return is intentional for streaming
func (c *Client) StartStream(ctx context.Context, chatID string, replyToMessageID int) (messaging.StreamSession, error) {
	return c.startStream(ctx, chatID, replyToMessageID)
}

func (c *Client) startStream(_ context.Context, chatID string, replyToMessageID int) (messaging.StreamSession, error) {
	chat := parseChatID(chatID)
	if chat == 0 {
		return nil, fmt.Errorf("telegram: invalid chat id %q", chatID)
	}
	m := tg.NewMessage(chat, "…")
	if replyToMessageID > 0 {
		m.ReplyToMessageID = replyToMessageID
	}
	sent, err := c.api.Send(m)
	if err != nil {
		c.log.Error().Err(err).Str("chat", chatID).Int64("chat_int", chat).Int("reply_to", replyToMessageID).Msg("telegram: stream start failed")
		return nil, fmt.Errorf("telegram: stream start: %w", err)
	}
	c.streamsMu.Lock()
	if c.activeStreams == nil {
		c.activeStreams = make(map[string]int)
	}
	c.activeStreams[chatID] = sent.MessageID
	c.streamsMu.Unlock()
	c.sendTyping(chatID)
	c.log.Debug().Str("chat", chatID).Int("msg_id", sent.MessageID).Int("reply_to", replyToMessageID).Msg("stream started")

	return &stream{
		client:   c,
		chatID:   chatID,
		msgID:    sent.MessageID,
		chat:     chat,
		lastSent: "…",
	}, nil
}

// formatText returns the wire-form text to send. When the
// configured ParseMode is HTML the LLM-emitted Markdown
// (**bold**, *italic*, `code`, [text](url)) is converted to
// the equivalent Telegram HTML entities. For MarkdownV2 or
// plain text the conversion is a no-op — MarkdownV2's strict
// escaping is the caller's problem.
//
// The per-message ParseMode (msg.ParseMode) takes precedence
// over the client default; passing "" falls back to c.cfg.
func (c *Client) formatText(text, msgMode string) string {
	mode := msgMode
	if mode == "" {
		mode = c.cfg.ParseMode
	}
	if mode == "HTML" || mode == "html" {
		return markdownToHTML(text)
	}

	return text
}

// Close stops accepting new updates and closes the receive channel.
func (c *Client) Close() error {
	if c.isOpen {
		return nil
	}
	c.isOpen = false
	close(c.recv)

	return nil
}

// --- internals ---

func parseChatID(s string) int64 {
	id, _ := strconv.ParseInt(s, 10, 64)
	return id
}

func (c *Client) sendTyping(chatID string) {
	chat := parseChatID(chatID)
	action := tg.NewChatAction(chat, tg.ChatTyping)
	_, _ = c.api.Send(action)
}

// typingLoop sends "typing..." to every chat with an open stream so
// Telegram displays the indicator even when individual chunks arrive
// faster than the typing-duration window.
func (c *Client) typingLoop(ctx context.Context) {
	ticker := time.NewTicker(4 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.streamsMu.Lock()
			for chatID := range c.activeStreams {
				c.sendTyping(chatID)
			}
			c.streamsMu.Unlock()
		}
	}
}

// Health implements messaging.Client. It is safe to call from any
// goroutine at any time and must not block.
func (c *Client) Health() messaging.HealthReport {
	c.healthMu.RLock()
	defer c.healthMu.RUnlock()

	return messaging.HealthReport{
		Name:      "telegram",
		State:     c.healthState,
		StartedAt: c.healthStartedAt,
		Message:   c.healthMessage,
	}
}

// setHealth updates the snapshot fields atomically. name is the
// transport identifier embedded in the report (passed so the helper
// can also serve clients whose Name() is non-trivial); message is a
// free-form detail string and must not contain secrets.
func (c *Client) setHealth(state messaging.HealthState, _ string, message string) {
	c.healthMu.Lock()
	defer c.healthMu.Unlock()
	c.healthState = state
	c.healthMessage = message
	if state == messaging.StateConnected && c.healthStartedAt.IsZero() {
		c.healthStartedAt = time.Now()
	}
}
