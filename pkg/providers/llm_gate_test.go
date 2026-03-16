package providers

import (
	"context"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// LLMGate — basic toggle
// ---------------------------------------------------------------------------

func TestLLMGate_EnableDisable(t *testing.T) {
	t.Parallel()
	g := NewLLMGate(true)
	if !g.IsEnabled() {
		t.Fatal("gate should start enabled")
	}

	g.SetEnabled(false)
	if g.IsEnabled() {
		t.Fatal("gate should be disabled after SetEnabled(false)")
	}

	g.SetEnabled(true)
	if !g.IsEnabled() {
		t.Fatal("gate should be enabled after SetEnabled(true)")
	}
}

// ---------------------------------------------------------------------------
// DisableUntil — future time
// ---------------------------------------------------------------------------

func TestLLMGate_DisableUntil_FutureTime(t *testing.T) {
	t.Parallel()
	g := NewLLMGate(true)
	defer g.Stop()

	resetAt := time.Now().Add(100 * time.Millisecond)
	g.DisableUntil(resetAt)

	if g.IsEnabled() {
		t.Fatal("gate should be disabled immediately after DisableUntil")
	}

	// Wait for the timer to fire.
	time.Sleep(250 * time.Millisecond)

	if !g.IsEnabled() {
		t.Fatal("gate should be re-enabled after reset_at elapsed")
	}
}

// ---------------------------------------------------------------------------
// DisableUntil — past time (pod reconnected after reset window)
// ---------------------------------------------------------------------------

func TestLLMGate_DisableUntil_PastTime(t *testing.T) {
	t.Parallel()
	g := NewLLMGate(true)
	defer g.Stop()

	pastTime := time.Now().Add(-1 * time.Hour)
	g.DisableUntil(pastTime)

	// Should re-enable immediately since reset_at is in the past.
	if !g.IsEnabled() {
		t.Fatal("gate should be re-enabled immediately when reset_at is in the past")
	}
}

// ---------------------------------------------------------------------------
// DisableUntil — zero time (indefinite disable)
// ---------------------------------------------------------------------------

func TestLLMGate_DisableUntil_ZeroTime(t *testing.T) {
	t.Parallel()
	g := NewLLMGate(true)
	defer g.Stop()

	g.DisableUntil(time.Time{})

	if g.IsEnabled() {
		t.Fatal("gate should be disabled after DisableUntil(zero)")
	}

	// Verify it stays disabled after a brief wait (no timer should fire).
	time.Sleep(100 * time.Millisecond)

	if g.IsEnabled() {
		t.Fatal("gate should remain disabled indefinitely with zero reset_at")
	}
}

// ---------------------------------------------------------------------------
// DisableUntil — second call replaces timer
// ---------------------------------------------------------------------------

func TestLLMGate_DisableUntil_ReplacesTimer(t *testing.T) {
	t.Parallel()
	g := NewLLMGate(true)
	defer g.Stop()

	// First call: reset far in the future.
	g.DisableUntil(time.Now().Add(10 * time.Second))

	// Second call: reset very soon — should replace the first timer.
	g.DisableUntil(time.Now().Add(100 * time.Millisecond))

	if g.IsEnabled() {
		t.Fatal("gate should be disabled after second DisableUntil")
	}

	time.Sleep(250 * time.Millisecond)

	if !g.IsEnabled() {
		t.Fatal("gate should be re-enabled by the replacement timer")
	}
}

// ---------------------------------------------------------------------------
// SetEnabled(true) cancels pending timer
// ---------------------------------------------------------------------------

func TestLLMGate_SetEnabled_CancelsTimer(t *testing.T) {
	t.Parallel()
	g := NewLLMGate(true)
	defer g.Stop()

	// Schedule re-enable 200ms from now.
	g.DisableUntil(time.Now().Add(200 * time.Millisecond))

	if g.IsEnabled() {
		t.Fatal("gate should be disabled")
	}

	// Explicit enable should cancel the timer.
	g.SetEnabled(true)
	if !g.IsEnabled() {
		t.Fatal("gate should be enabled after SetEnabled(true)")
	}

	// Disable again without a timer.
	g.SetEnabled(false)

	// Wait past the original reset_at — timer should NOT fire.
	time.Sleep(300 * time.Millisecond)

	if g.IsEnabled() {
		t.Fatal("cancelled timer should not have re-enabled the gate")
	}
}

// ---------------------------------------------------------------------------
// CancelReset — standalone
// ---------------------------------------------------------------------------

func TestLLMGate_CancelReset(t *testing.T) {
	t.Parallel()
	g := NewLLMGate(true)
	defer g.Stop()

	g.DisableUntil(time.Now().Add(100 * time.Millisecond))
	g.CancelReset()

	// Wait past the reset_at.
	time.Sleep(200 * time.Millisecond)

	if g.IsEnabled() {
		t.Fatal("gate should stay disabled after CancelReset")
	}
}

// ---------------------------------------------------------------------------
// Stop — cleans up timer
// ---------------------------------------------------------------------------

func TestLLMGate_Stop(t *testing.T) {
	t.Parallel()
	g := NewLLMGate(true)

	g.DisableUntil(time.Now().Add(100 * time.Millisecond))
	g.Stop()

	time.Sleep(200 * time.Millisecond)

	if g.IsEnabled() {
		t.Fatal("gate should stay disabled after Stop cancelled the timer")
	}
}

// ---------------------------------------------------------------------------
// LLMGateProvider — blocks when disabled
// ---------------------------------------------------------------------------

type mockGateProvider struct {
	called bool
}

func (m *mockGateProvider) Chat(
	ctx context.Context,
	messages []Message,
	tools []ToolDefinition,
	model string,
	options map[string]any,
) (*LLMResponse, error) {
	m.called = true
	return &LLMResponse{Content: "hello"}, nil
}

func (m *mockGateProvider) GetDefaultModel() string { return "mock" }

func TestLLMGateProvider_BlocksWhenDisabled(t *testing.T) {
	t.Parallel()
	mock := &mockGateProvider{}
	gate := NewLLMGate(false)
	p := NewLLMGateProvider(mock, gate)

	resp, err := p.Chat(context.Background(), nil, nil, "model", nil)
	if err != ErrLLMDisabled {
		t.Fatalf("expected ErrLLMDisabled, got %v", err)
	}
	if resp != nil {
		t.Fatal("expected nil response")
	}
	if mock.called {
		t.Fatal("delegate should not have been called")
	}
}

func TestLLMGateProvider_PassesWhenEnabled(t *testing.T) {
	t.Parallel()
	mock := &mockGateProvider{}
	gate := NewLLMGate(true)
	p := NewLLMGateProvider(mock, gate)

	resp, err := p.Chat(context.Background(), nil, nil, "model", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp == nil || resp.Content != "hello" {
		t.Fatalf("unexpected response: %v", resp)
	}
	if !mock.called {
		t.Fatal("delegate should have been called")
	}
}

func TestLLMGateProvider_GetDefaultModel(t *testing.T) {
	t.Parallel()
	mock := &mockGateProvider{}
	gate := NewLLMGate(true)
	p := NewLLMGateProvider(mock, gate)

	if got := p.GetDefaultModel(); got != "mock" {
		t.Fatalf("expected 'mock', got %q", got)
	}
}
