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
	client   *Client
	chatID   string
	msgID    int
	chat     int64
	ctx      context.Context
	closed   bool
	lastSent string
}

func (s *stream) Append(ctx context.Context, text string) error {
	if s.closed {
		return fmt.Errorf("stream closed")
	}
	if s.lastSent == text {
		return nil
	}
	wire := s.client.formatText(text, "")
	e := tg.EditMessageTextConfig{
		BaseEdit: tg.BaseEdit{
			ChatID:    s.chat,
			MessageID: s.msgID,
		},
		Text: wire,
	}
	if s.client.cfg.ParseMode != "" {
		e.ParseMode = s.client.cfg.ParseMode
	}
	if _, err := s.client.api.Send(e); err != nil {
		if isMessageNotModified(err) {
			s.lastSent = text
			s.client.log.Debug().
				Str("chat", s.chatID).
				Int("msg_id", s.msgID).
				Int("text_len", len(text)).
				Msg("stream edit: no-op (content unchanged)")
			return nil
		}
		s.client.log.Error().
			Err(err).
			Str("chat", s.chatID).
			Int("msg_id", s.msgID).
			Int("text_len", len(text)).
			Msg("stream edit failed")
		return err
	}
	s.lastSent = text
	return nil
}

func (s *stream) Final(ctx context.Context, text string) error {
	if s.closed {
		return nil
	}
	if err := s.Append(ctx, text); err != nil {
		return err
	}
	s.closed = true
	s.lastSent = ""
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
	if since := time.Since(t.last); since < t.minDelay {
		wait := t.minDelay - since
		t.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		t.mu.Lock()
	}
	t.last = time.Now()
	t.mu.Unlock()
	return t.inner.Append(ctx, text)
}

func (t *ThrottledStream) Final(ctx context.Context, text string) error {
	t.mu.Lock()
	t.last = time.Now()
	t.mu.Unlock()
	return t.inner.Final(ctx, text)
}
