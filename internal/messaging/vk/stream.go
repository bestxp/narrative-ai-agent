package vk

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/SevereCloud/vksdk/v3/api"

	"github.com/bestxp/narrative-ai-agent/internal/messaging"
)

type stream struct {
	client   *Client
	chatID   string
	peerID   int
	msgID    int
	groupID  int
	ctx      context.Context
	closed   bool
	lastSent string
}

func (s *stream) Append(ctx context.Context, text string) error {
	if s.closed {
		return fmt.Errorf("vk: stream closed")
	}
	if s.lastSent == text {
		return nil
	}
	wire := text
	if strings.TrimSpace(wire) == "" {
		return nil
	}
	if len(wire) > maxVKMessageLen {
		chunks := splitForVK(wire)
		wire = chunks[0]
		for _, tail := range chunks[1:] {
			_, _ = s.client.vk.MessagesSend(api.Params{
				"peer_id":   s.peerID,
				"message":   tail,
				"random_id": 0,
				"group_id":  s.groupID,
			})
		}
	}
	_, err := s.client.vk.MessagesEdit(api.Params{
		"peer_id":    s.peerID,
		"message":    wire,
		"message_id": s.msgID,
		"group_id":   s.groupID,
	})
	if err != nil {
		if strings.Contains(err.Error(), "message is not modified") || strings.Contains(err.Error(), "same message") {
			s.lastSent = text
			s.client.log.Debug().Str("chat", s.chatID).Int("msg_id", s.msgID).Msg("vk: stream edit: no-op (content unchanged)")
			return nil
		}
		s.client.log.Error().Err(err).Str("chat", s.chatID).Int("msg_id", s.msgID).Msg("vk: stream edit failed")
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
	s.client.mu.Lock()
	delete(s.client.activeStreams, s.chatID)
	s.client.mu.Unlock()
	return nil
}

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
