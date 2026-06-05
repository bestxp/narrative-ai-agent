package telegram

import (
	"context"
	"fmt"
	"sync"
	"time"

	tg "github.com/go-telegram-bot-api/telegram-bot-api"

	"narrative/internal/messaging"
)

// stream is a streaming session: it edits a single Telegram message
// over time. Append replaces the message; Final makes the last
// replacement and removes the chat from the active set.
type stream struct {
	client *Client
	chatID string
	msgID  int
	chat   int64
	ctx    context.Context
	closed bool
}

func (s *stream) Append(ctx context.Context, text string) error {
	if s.closed {
		return fmt.Errorf("stream closed")
	}
	e := tg.EditMessageTextConfig{
		BaseEdit: tg.BaseEdit{
			ChatID:    s.chat,
			MessageID: s.msgID,
		},
		Text: text,
	}
	if s.client.cfg.ParseMode != "" {
		e.ParseMode = s.client.cfg.ParseMode
	}
	_, err := s.client.api.Send(e)
	return err
}

func (s *stream) Final(ctx context.Context, text string) error {
	if s.closed {
		return nil
	}
	s.closed = true
	if err := s.Append(ctx, text); err != nil {
		return err
	}
	s.client.streamsMu.Lock()
	delete(s.client.activeStreams, s.chatID)
	s.client.streamsMu.Unlock()
	return nil
}

// ThrottledStream is a wrapper that rate-limits edits to ~1.5 per
// second. Telegram rate-limits aggressively; spamming edits leads
// to 429s and broken streams.
type ThrottledStream struct {
	inner    messaging.StreamSession
	minDelay time.Duration
	last     time.Time
	mu       sync.Mutex
}

func NewThrottledStream(inner messaging.StreamSession) *ThrottledStream {
	return &ThrottledStream{inner: inner, minDelay: 700 * time.Millisecond}
}

func (t *ThrottledStream) Append(ctx context.Context, text string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if since := time.Since(t.last); since < t.minDelay {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(t.minDelay - since):
		}
	}
	t.last = time.Now()
	return t.inner.Append(ctx, text)
}

func (t *ThrottledStream) Final(ctx context.Context, text string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.last = time.Now()
	return t.inner.Final(ctx, text)
}
