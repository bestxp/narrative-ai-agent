package vk

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/SevereCloud/vksdk/v3/api"
	"github.com/SevereCloud/vksdk/v3/events"
	"github.com/SevereCloud/vksdk/v3/longpoll-bot"
	"github.com/rs/zerolog"

	"github.com/bestxp/narrative-ai-agent/internal/messaging"
)

type Config struct {
	AccessToken      string
	GroupID          int
	AllowedUserIDs   []int
	PollingWait      int
	DisableStreaming bool
}

type Client struct {
	cfg           Config
	vk            *api.VK
	lp            *longpoll.LongPoll
	log           zerolog.Logger
	recv          chan messaging.IncomingMessage
	mu            sync.Mutex
	activeStreams map[string]int

	// healthMu guards the HealthReport snapshot. Health() is
	// called from the HTTP probe goroutine on a 1-5s cadence.
	healthMu        sync.RWMutex
	healthState     messaging.HealthState
	healthStartedAt time.Time
	healthMessage   string
}

func New(cfg Config, log zerolog.Logger) (*Client, error) {
	if cfg.AccessToken == "" {
		return nil, errors.New("vk: empty access token")
	}
	if cfg.GroupID == 0 {
		return nil, errors.New("vk: group_id is required")
	}
	if cfg.PollingWait == 0 {
		cfg.PollingWait = 25
	}

	vk := api.NewVK(cfg.AccessToken)
	lp, err := longpoll.NewLongPoll(vk, cfg.GroupID)
	if err != nil {
		return nil, fmt.Errorf("vk: longpoll init: %w", err)
	}

	c := &Client{
		cfg:           cfg,
		vk:            vk,
		lp:            lp,
		log:           log.With().Str("transport", "vk").Logger(),
		recv:          make(chan messaging.IncomingMessage, 64),
		activeStreams: make(map[string]int),
		healthState:   messaging.StateStarting,
	}

	lp.MessageNew(c.onMessageNew)

	return c, nil
}

func (c *Client) Name() string { return "vk" }

func (c *Client) Recv() <-chan messaging.IncomingMessage {
	return c.recv
}

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

func (c *Client) SetCommands(_ context.Context, _ []messaging.BotCommand) error {
	return nil
}

func (c *Client) onMessageNew(ctx context.Context, obj events.MessageNewObject) {
	msg := obj.Message

	if msg.Out || msg.FromID <= 0 {
		return
	}

	senderID := strconv.Itoa(msg.FromID)
	if !c.IsAllowed(senderID) {
		c.log.Warn().Int("from_id", msg.FromID).Msg("vk: rejected user")
		return
	}

	text := strings.TrimSpace(msg.Text)
	peerID := strconv.Itoa(msg.PeerID)

	incoming := messaging.IncomingMessage{
		Sender: messaging.Sender{
			ID:   senderID,
			Name: "",
		},
		ChatID:    peerID,
		Text:      text,
		MessageID: msg.ConversationMessageID,
	}
	if strings.HasPrefix(text, "/") || strings.HasPrefix(text, "!") {
		parts := strings.Fields(text)
		incoming.Command = strings.TrimPrefix(parts[0], "/")
		incoming.Command = strings.TrimPrefix(incoming.Command, "!")
		if len(parts) > 1 {
			incoming.Args = parts[1:]
		}
	}

	c.recv <- incoming
}

func (c *Client) Run(ctx context.Context) error {
	c.log.Info().Int("group_id", c.cfg.GroupID).Msg("vk bot started")

	go c.typingLoop(ctx)

	backoff := time.Second
	const maxBackoff = 30 * time.Second

	for {
		c.setHealth(messaging.StateConnected, "", "")
		errCh := make(chan error, 1)
		go func() {
			if err := c.lp.Run(); err != nil {
				errCh <- err
			}
			close(errCh)
		}()

		select {
		case <-ctx.Done():
			c.setHealth(messaging.StateStopped, "", "ctx cancelled")
			c.lp.Shutdown()
			c.log.Info().Msg("vk: shutdown")
			return nil
		case err := <-errCh:
			c.setHealth(messaging.StateReconnect, "", err.Error())
			c.log.Error().
				Err(err).
				Dur("backoff", backoff).
				Msg("vk: longpoll error, reconnecting")
			select {
			case <-ctx.Done():
				c.setHealth(messaging.StateStopped, "", "ctx cancelled")
				c.lp.Shutdown()
				c.log.Info().Msg("vk: shutdown")
				return nil
			case <-time.After(backoff):
			}
			if backoff < maxBackoff {
				backoff *= 2
			}
		}
	}
}

func (c *Client) Send(ctx context.Context, msg messaging.OutgoingMessage) error {
	peerID, err := strconv.Atoi(msg.ChatID)
	if err != nil {
		return fmt.Errorf("vk: invalid peer_id %q: %w", msg.ChatID, err)
	}

	text := msg.Text
	chunks := splitForVK(text)
	for i, chunk := range chunks {
		params := api.Params{
			"peer_id":   peerID,
			"message":   chunk,
			"random_id": 0,
			"group_id":  c.cfg.GroupID,
		}
		if i == 0 && msg.ReplyToMessageID > 0 {
			params["reply_to"] = msg.ReplyToMessageID
		}
		if _, sendErr := c.vk.MessagesSend(params); sendErr != nil {
			c.log.Error().Err(sendErr).Str("peer", msg.ChatID).Int("chunk", i).Msg("vk: send failed")
			return fmt.Errorf("send_chunks: MessagesSend failed: %w", sendErr)
		}
	}
	return nil
}

func (c *Client) StartStream(ctx context.Context, chatID string, replyToMessageID int) (messaging.StreamSession, error) {
	if c.cfg.DisableStreaming {
		return nil, nil
	}
	peerID, err := strconv.Atoi(chatID)
	if err != nil {
		return nil, fmt.Errorf("vk: invalid peer_id %q: %w", chatID, err)
	}

	params := api.Params{
		"peer_id":   peerID,
		"message":   "\u2026",
		"random_id": 0,
		"group_id":  c.cfg.GroupID,
	}
	msgID, err := c.vk.MessagesSend(params)
	if err != nil {
		c.log.Error().Err(err).Str("chat", chatID).Msg("vk: stream start failed")
		return nil, fmt.Errorf("vk: stream start: %w", err)
	}

	c.mu.Lock()
	c.activeStreams[chatID] = msgID
	c.mu.Unlock()

	c.log.Debug().Str("chat", chatID).Int("msg_id", msgID).Msg("vk: stream started")

	return NewThrottledStream(ctx, &stream{
		client:   c,
		chatID:   chatID,
		peerID:   peerID,
		msgID:    msgID,
		groupID:  c.cfg.GroupID,
		lastSent: "\u2026",
	}), nil
}

func (c *Client) sendTyping(chatID string) {
	peerID, err := strconv.Atoi(chatID)
	if err != nil {
		return
	}
	_, _ = c.vk.MessagesSetActivity(api.Params{
		"peer_id":  peerID,
		"type":     "typing",
		"group_id": c.cfg.GroupID,
	})
}

func (c *Client) typingLoop(ctx context.Context) {
	ticker := time.NewTicker(4 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.mu.Lock()
			for chatID := range c.activeStreams {
				c.sendTyping(chatID)
			}
			c.mu.Unlock()
		}
	}
}

const maxVKMessageLen = 4096

func splitForVK(text string) []string {
	runes := []rune(text)
	if len(runes) <= maxVKMessageLen {
		return []string{text}
	}
	out := make([]string, 0, 2)
	rest := string(runes)
	for runeCount := len(runes); runeCount > maxVKMessageLen; runeCount = len([]rune(rest)) {
		head := string([]rune(rest)[:maxVKMessageLen])
		var cut int
		if i := strings.LastIndex(head, "\n\n"); i > 0 {
			cut = i + len("\n\n")
		} else if i := strings.LastIndex(head, "\n"); i > 0 {
			cut = i + len("\n")
		} else {
			cut = len(head)
		}
		out = append(out, rest[:cut])
		rest = rest[cut:]
	}
	if rest != "" {
		out = append(out, rest)
	}
	return out
}

// Health implements messaging.Client. It is safe to call from any
// goroutine at any time and must not block.
func (c *Client) Health() messaging.HealthReport {
	c.healthMu.RLock()
	defer c.healthMu.RUnlock()
	return messaging.HealthReport{
		Name:      "vk",
		State:     c.healthState,
		StartedAt: c.healthStartedAt,
		Message:   c.healthMessage,
	}
}

// setHealth atomically updates the snapshot fields. message is
// a free-form detail string and must not contain secrets.
func (c *Client) setHealth(state messaging.HealthState, _ string, message string) {
	c.healthMu.Lock()
	defer c.healthMu.Unlock()
	c.healthState = state
	c.healthMessage = message
	if state == messaging.StateConnected && c.healthStartedAt.IsZero() {
		c.healthStartedAt = time.Now()
	}
}
