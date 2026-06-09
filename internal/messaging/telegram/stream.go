package telegram

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	tg "github.com/go-telegram-bot-api/telegram-bot-api"

	"github.com/bestxp/narrative-ai-agent/internal/messaging"
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
	// When the model rounds produce no visible text (e.g.
	// finish_reason=tool_calls with empty content), the
	// formatted wire text can be empty or contain only HTML
	// tags. Sending that to Telegram triggers "Bad Request:
	// message text is empty". Skip the edit entirely; the
	// next Append call with real content will update the
	// placeholder.
	if strings.TrimSpace(stripHTMLTags(wire)) == "" {
		return nil
	}
	// Telegram caps messages at 4096 chars; if the LLM
	// produced a 7k-block in one go we have to split it.
	// The first chunk fits in the placeholder message,
	// subsequent chunks go to fresh messages.
	if len(wire) > maxTelegramMessageLen {
		chunks := splitForTelegram(wire)
		wire = chunks[0]
		// The remainder we send after the stream Final
		// (Final calls Append with the full text — by the
		// time Final runs the text is the complete
		// assembly). For Append, we only see a growing
		// prefix, so any chunks beyond the first are
		// appended to the visible stream as new messages
		// now.
		for _, tail := range chunks[1:] {
			m := tg.NewMessage(s.chat, tail)
			if s.client.cfg.ParseMode != "" {
				m.ParseMode = s.client.cfg.ParseMode
			}
			if _, err := s.client.api.Send(m); err != nil {
				s.client.log.Error().
					Err(err).
					Str("chat", s.chatID).
					Int("text_len", len(tail)).
					Msg("stream tail send failed")
			}
		}
	}
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
		if isMessageTooLong(err) {
			// Defensive: the pre-split above should have
			// caught this, but if the formatted wire text
			// grew past the cap (markup expansion) we
			// retry with the first chunk and queue the
			// rest.
			chunks := splitForTelegram(wire)
			wire = chunks[0]
			e.Text = wire
			if _, retryErr := s.client.api.Send(e); retryErr != nil {
				s.client.log.Error().
					Err(retryErr).
					Str("chat", s.chatID).
					Int("text_len", len(wire)).
					Msg("stream edit retry after split failed")
				return retryErr
			}
			for _, tail := range chunks[1:] {
				m := tg.NewMessage(s.chat, tail)
				if s.client.cfg.ParseMode != "" {
					m.ParseMode = s.client.cfg.ParseMode
				}
				if _, sendErr := s.client.api.Send(m); sendErr != nil {
					s.client.log.Error().
						Err(sendErr).
						Str("chat", s.chatID).
						Int("text_len", len(tail)).
						Msg("stream tail send after split failed")
				}
			}
		} else {
			s.client.log.Error().
				Err(err).
				Str("chat", s.chatID).
				Int("msg_id", s.msgID).
				Int("text_len", len(text)).
				Msg("stream edit failed")
			return err
		}
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
