// Package telegram implements the messaging.Client interface for the
// Telegram Bot API. It uses long-polling; switching to webhooks is a
// future optimisation, not a refactor.
package telegram

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	tg "github.com/go-telegram-bot-api/telegram-bot-api"
	"github.com/rs/zerolog"

	"narrative/internal/messaging"
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
		cfg:  cfg,
		api:  api,
		log:  log.With().Str("transport", "telegram").Logger(),
		recv: make(chan messaging.IncomingMessage, 64),
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
	for _, allow := range c.cfg.AllowedUserIDs {
		if allow == id {
			return true
		}
	}
	return false
}

// Run implements messaging.Client. It blocks until ctx is cancelled
// or the polling channel closes.
func (c *Client) Run(ctx context.Context) error {
	u := tg.NewUpdate(0)
	u.Timeout = c.cfg.PollingTimeout
	updates, err := c.api.GetUpdatesChan(u)
	if err != nil {
		return fmt.Errorf("telegram: get updates: %w", err)
	}
	c.log.Info().Str("username", c.api.Self.UserName).Msg("bot started")

	// Typing ticker: refreshes "typing..." for chats with active streams.
	go c.typingLoop(ctx)

	for {
		select {
		case <-ctx.Done():
			c.log.Info().Msg("shutdown")
			return nil
		case upd, ok := <-updates:
			if !ok {
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
				ChatID: strconv.FormatInt(upd.Message.Chat.ID, 10),
				Text:   text,
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
func (c *Client) Send(ctx context.Context, msg messaging.OutgoingMessage) error {
	m := tg.NewMessage(parseChatID(msg.ChatID), msg.Text)
	if msg.ParseMode != "" {
		m.ParseMode = msg.ParseMode
	} else if c.cfg.ParseMode != "" {
		m.ParseMode = c.cfg.ParseMode
	}
	_, err := c.api.Send(m)
	return err
}

// StartStream implements messaging.Client.
func (c *Client) StartStream(ctx context.Context, chatID string) (messaging.StreamSession, error) {
	chat := parseChatID(chatID)
	m := tg.NewMessage(chat, "…")
	sent, err := c.api.Send(m)
	if err != nil {
		return nil, fmt.Errorf("telegram: stream start: %w", err)
	}
	c.streamsMu.Lock()
	if c.activeStreams == nil {
		c.activeStreams = make(map[string]int)
	}
	c.activeStreams[chatID] = sent.MessageID
	c.streamsMu.Unlock()
	c.sendTyping(chatID)
	return &stream{
		client: c,
		chatID: chatID,
		msgID:  sent.MessageID,
		chat:   chat,
		ctx:    ctx,
	}, nil
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
