package handler

import (
	"fmt"
	"sync"
)

// ChatMutexPool serialises HandleOne per chatID so two
// messages from the same player — or from a Telegram +
// Discord client pointing at the same logical chat — are
// processed strictly one at a time. The map is grown on
// demand; load is atomic.
//
// Why a dedicated type rather than a process-wide sync.Map:
//
//   - main.go originally held `var chatMu sync.Map` as a
//     global. The package-local pool keeps the global
//     surface explicit (no more `nolint:gochecknoglobals`)
//     and lets us swap the backing store in tests if needed.
//   - The mutex is keyed by chatID, which is a transport-
//     supplied string. We do not deduplicate / evict —
//     long-running deployments accumulate a few thousand
//     entries, each a single *sync.Mutex, which is fine.
type ChatMutexPool struct {
	mu sync.Map // map[string]*sync.Mutex
}

// NewChatMutexPool returns an empty pool. The zero value
// is also valid; the constructor exists for symmetry with
// the other handler.New* helpers.
func NewChatMutexPool() *ChatMutexPool {
	return &ChatMutexPool{}
}

// Lock returns the mutex for the given chatID. The first
// call for a chatID installs a fresh *sync.Mutex; subsequent
// calls return the same instance. The mutex is locked by
// the caller (and unlocked by `mu.Unlock()` / `defer
// mu.Unlock()`).
func (p *ChatMutexPool) Lock(chatID string) *sync.Mutex {
	v, _ := p.mu.LoadOrStore(chatID, &sync.Mutex{})

	mu, ok := v.(*sync.Mutex)
	if !ok {
		panic(fmt.Sprintf("ChatMutexPool: expected *sync.Mutex, got %T", v))
	}

	return mu
}
