package providers

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sipeed/picoclaw/pkg/logger"
)

// ErrLLMDisabled is returned by LLMGateProvider when the gate is closed.
var ErrLLMDisabled = errors.New("LLM calls are disabled")

// LLMGate is a thread-safe toggle that controls whether LLM calls are allowed.
// It is shared between the provider wrapper (which checks it) and the WebSocket
// channel (which sets it on backend commands).
type LLMGate struct {
	enabled    atomic.Bool
	mu         sync.Mutex  // guards resetTimer
	resetTimer *time.Timer // scheduled auto-re-enable; nil when no timer is active
}

// NewLLMGate creates a gate with the given initial state.
func NewLLMGate(initiallyEnabled bool) *LLMGate {
	g := &LLMGate{}
	g.enabled.Store(initiallyEnabled)
	return g
}

// SetEnabled sets the gate state and cancels any pending reset timer.
// An explicit enable from the backend is authoritative and overrides any
// scheduled auto-re-enable.
func (g *LLMGate) SetEnabled(v bool) {
	g.cancelResetLocked()
	g.enabled.Store(v)
}

func (g *LLMGate) IsEnabled() bool { return g.enabled.Load() }

// DisableUntil disables the gate immediately and, if resetAt is non-zero and
// in the future, schedules automatic re-enablement at that time. If resetAt is
// in the past (e.g. pod reconnected after the reset window), the gate is
// re-enabled immediately. A zero resetAt disables indefinitely.
//
// Calling DisableUntil again replaces any previously scheduled timer.
func (g *LLMGate) DisableUntil(resetAt time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()

	// Cancel any existing timer before changing state.
	if g.resetTimer != nil {
		g.resetTimer.Stop()
		g.resetTimer = nil
	}

	g.enabled.Store(false)

	if resetAt.IsZero() {
		return
	}

	delay := time.Until(resetAt)
	if delay <= 0 {
		// Reset time already passed — re-enable immediately.
		g.enabled.Store(true)
		logger.InfoCF("llm_gate", "reset_at is in the past, re-enabling immediately", map[string]any{
			"reset_at": resetAt.Format(time.RFC3339),
		})
		return
	}

	g.resetTimer = time.AfterFunc(delay, func() {
		g.mu.Lock()
		g.resetTimer = nil
		g.mu.Unlock()

		g.enabled.Store(true)
		logger.InfoCF("llm_gate", "LLM gate auto-re-enabled by reset timer", map[string]any{
			"reset_at": resetAt.Format(time.RFC3339),
		})
	})

	logger.InfoCF("llm_gate", "Scheduled LLM gate re-enable", map[string]any{
		"reset_at": resetAt.Format(time.RFC3339),
		"delay":    delay.String(),
	})
}

// CancelReset cancels any pending reset timer without changing the gate state.
func (g *LLMGate) CancelReset() {
	g.cancelResetLocked()
}

// Stop cancels any pending reset timer. Call on shutdown.
func (g *LLMGate) Stop() {
	g.cancelResetLocked()
}

func (g *LLMGate) cancelResetLocked() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.resetTimer != nil {
		g.resetTimer.Stop()
		g.resetTimer = nil
	}
}

// LLMGateProvider wraps an LLMProvider and rejects Chat() calls when the gate
// is closed. This is independent from usage tracking — it sits earlier in the
// provider chain so a disabled gate prevents both token consumption and usage
// reporting.
type LLMGateProvider struct {
	delegate LLMProvider
	gate     *LLMGate
}

func NewLLMGateProvider(delegate LLMProvider, gate *LLMGate) *LLMGateProvider {
	return &LLMGateProvider{delegate: delegate, gate: gate}
}

func (p *LLMGateProvider) Chat(
	ctx context.Context,
	messages []Message,
	tools []ToolDefinition,
	model string,
	options map[string]any,
) (*LLMResponse, error) {
	if !p.gate.IsEnabled() {
		return nil, ErrLLMDisabled
	}
	return p.delegate.Chat(ctx, messages, tools, model, options)
}

func (p *LLMGateProvider) GetDefaultModel() string {
	return p.delegate.GetDefaultModel()
}
