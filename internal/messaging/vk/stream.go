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

// ThrottledStream batches text updates and flushes them to VK at
// most once every minDelay.  Append only accumulates — it never
// blocks.  A background ticker drains the buffer periodically; Final
// drains whatever is left and shuts the ticker down.
type ThrottledStream struct {
	inner    messaging.StreamSession
	minDelay time.Duration
	mu       sync.Mutex
	buffer   string
	ticker   *time.Ticker
	stop     chan struct{}
	wg       sync.WaitGroup
}

func NewThrottledStream(inner messaging.StreamSession) *ThrottledStream {
	t := &ThrottledStream{
		inner:    inner,
		minDelay: 700 * time.Millisecond,
		stop:     make(chan struct{}),
	}
	t.ticker = time.NewTicker(t.minDelay)
	t.wg.Add(1)
	go t.loop()
	return t
}

func (t *ThrottledStream) loop() {
	defer t.wg.Done()
	for {
		select {
		case <-t.ticker.C:
			t.flush()
		case <-t.stop:
			t.flush()
			return
		}
	}
}

// flush sends the current buffer to the inner stream and clears it.
func (t *ThrottledStream) flush() {
	t.mu.Lock()
	text := t.buffer
	t.buffer = ""
	t.mu.Unlock()

	if text == "" {
		return
	}
	// Best-effort flush; errors are logged by the inner stream.
	_ = t.inner.Append(context.Background(), text)
}

func (t *ThrottledStream) Append(ctx context.Context, text string) error {
	if text == "" {
		return nil
	}
	t.mu.Lock()
	t.buffer = text
	t.mu.Unlock()
	return nil
}

func (t *ThrottledStream) Final(ctx context.Context, text string) error {
	// Stop the ticker and wait for the last flush.
	t.ticker.Stop()
	close(t.stop)
	t.wg.Wait()

	// Flush any remaining text directly.
	t.mu.Lock()
	remaining := t.buffer
	t.buffer = ""
	t.mu.Unlock()

	if remaining != "" {
		if err := t.inner.Append(ctx, remaining); err != nil {
			return err
		}
	}
	return t.inner.Final(ctx, text)
}
