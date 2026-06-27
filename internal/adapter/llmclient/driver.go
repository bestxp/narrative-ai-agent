// Package llmclient adapts internal/adapter/llm.Driver (which
// owns resource lifecycles via Close) to the narrower
// usecase.LLMClient interface (Stream only).
//
// Why this adapter lives in its own package:
//
//   - The usecase layer is unaware of driver lifetimes; the
//     composition root (cmd/bot/app) owns Close() and calls
//     it on shutdown.
//   - main.go originally held the shim as a one-line struct.
//     Promoting it to a package makes it testable in isolation
//     and gives future drivers (local llama.cpp, Bedrock,
//     ...) a single seam to plug into without touching
//     usecase.GM.
//
// Usage:
//
//	driver := llmopenai.New(rc, log)
//	gmCli := llmclient.New(driver)
//	gm := usecase.NewGM(..., gmCli, ...)
//
// Then on shutdown:
//
//	_ = driver.Close() // composition root owns this.
package llmclient

import (
	"context"
	"fmt"

	"github.com/bestxp/narrative-ai-agent/internal/adapter/llm"
)

// Driver is the usecase.LLMClient adapter over an llm.Driver.
// It implements usecase.LLMClient (Stream only).
type Driver struct {
	driver llm.Driver
}

// New wraps an llm.Driver into the usecase.LLMClient surface.
// The returned Driver does NOT take ownership of the underlying
// llm.Driver — Close() on the wrapped driver is the caller's
// responsibility (composition root).
func New(d llm.Driver) *Driver {
	return &Driver{driver: d}
}

// Stream forwards to the underlying llm.Driver.Stream and
// wraps any non-nil error so the call site stays free of
// context-free strings ("driver stream: ...").
func (d *Driver) Stream(ctx context.Context, req llm.ChatRequest, onChunk func(llm.Chunk) error) error {
	if err := d.driver.Stream(ctx, req, onChunk); err != nil {
		return fmt.Errorf("driver stream: %w", err)
	}

	return nil
}

// Compile-time check: Driver satisfies usecase.LLMClient.
//
// usecase.LLMClient is declared in a downstream package; we
// rely on a structural match (same method set) — the actual
// interface assertion happens at the wire site that stores
// the adapter in a usecase.LLMClient-typed field. golangci-lint
// catches drift via the staticcheck `iface` checker at the
// consumer.
var _ interface {
	Stream(ctx context.Context, req llm.ChatRequest, onChunk func(llm.Chunk) error) error
} = (*Driver)(nil)
