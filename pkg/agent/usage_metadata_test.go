package agent

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/providers"
)

// usageMockProvider returns a fixed LLMResponse with known token counts.
type usageMockProvider struct {
	response string
	usage    *providers.UsageInfo
}

func (m *usageMockProvider) Chat(
	ctx context.Context,
	messages []providers.Message,
	toolDefs []providers.ToolDefinition,
	model string,
	opts map[string]any,
) (*providers.LLMResponse, error) {
	return &providers.LLMResponse{
		Content:   m.response,
		ToolCalls: []providers.ToolCall{},
		Usage:     m.usage,
	}, nil
}

func (m *usageMockProvider) GetDefaultModel() string { return "mock-usage-model" }

// drainOutbound reads up to n messages from the bus outbound channel within timeout.
func drainOutbound(msgBus *bus.MessageBus, n int, timeout time.Duration) []bus.OutboundMessage {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var msgs []bus.OutboundMessage
	for i := 0; i < n; i++ {
		msg, ok := msgBus.SubscribeOutbound(ctx)
		if !ok {
			break
		}
		msgs = append(msgs, msg)
	}
	return msgs
}

func newUsageTestAgentLoop(t *testing.T, provider providers.LLMProvider) (*AgentLoop, *bus.MessageBus, func()) {
	t.Helper()
	tmpDir, err := os.MkdirTemp("", "agent-usage-test-*")
	if err != nil {
		t.Fatalf("os.MkdirTemp: %v", err)
	}
	cfg := &config.Config{
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{
				Workspace:         tmpDir,
				Model:             "test-model",
				MaxTokens:         4096,
				MaxToolIterations: 10,
			},
		},
	}
	msgBus := bus.NewMessageBus()
	al := NewAgentLoop(cfg, msgBus, provider)
	return al, msgBus, func() { os.RemoveAll(tmpDir) }
}

// TestUsageMetadata_ProcessMessage_ReturnsUsage verifies processMessage propagates
// provider usage to its caller (the run() loop then attaches it to the outbound message).
func TestUsageMetadata_ProcessMessage_ReturnsUsage(t *testing.T) {
	usage := &providers.UsageInfo{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150}
	provider := &usageMockProvider{response: "hello", usage: usage}
	al, _, cleanup := newUsageTestAgentLoop(t, provider)
	defer cleanup()

	msg := bus.InboundMessage{
		Channel:    "test",
		SenderID:   "user1",
		ChatID:     "chat1",
		Content:    "hi",
		SessionKey: "sess-1",
	}
	_, gotUsage, err := al.processMessage(context.Background(), msg)
	if err != nil {
		t.Fatalf("processMessage: %v", err)
	}
	if gotUsage == nil {
		t.Fatal("expected non-nil usage from processMessage, got nil")
	}
	if gotUsage.PromptTokens != 100 {
		t.Errorf("PromptTokens: want 100, got %d", gotUsage.PromptTokens)
	}
	if gotUsage.CompletionTokens != 50 {
		t.Errorf("CompletionTokens: want 50, got %d", gotUsage.CompletionTokens)
	}
	if gotUsage.TotalTokens != 150 {
		t.Errorf("TotalTokens: want 150, got %d", gotUsage.TotalTokens)
	}
}

// TestUsageMetadata_ProcessDirectWithChannel_PublishesMetadata verifies Bug #4 fix:
// ProcessDirectWithChannel publishes the agent response WITH usage metadata to the bus.
func TestUsageMetadata_ProcessDirectWithChannel_PublishesMetadata(t *testing.T) {
	usage := &providers.UsageInfo{PromptTokens: 200, CompletionTokens: 80, TotalTokens: 280}
	provider := &usageMockProvider{response: "cron result", usage: usage}
	al, msgBus, cleanup := newUsageTestAgentLoop(t, provider)
	defer cleanup()

	_, err := al.ProcessDirectWithChannel(
		context.Background(),
		"run cron task",
		"cron-sess-1",
		"websocket_client",
		"user-abc",
	)
	if err != nil {
		t.Fatalf("ProcessDirectWithChannel: %v", err)
	}

	msgs := drainOutbound(msgBus, 1, 2*time.Second)
	if len(msgs) == 0 {
		t.Fatal("expected 1 outbound message, got none")
	}

	m := msgs[0]
	if m.Content != "cron result" {
		t.Errorf("Content: want 'cron result', got %q", m.Content)
	}
	if m.Metadata == nil {
		t.Fatal("expected Metadata to be non-nil")
	}
	if m.Metadata["usage_prompt_tokens"] == "" {
		t.Error("missing usage_prompt_tokens in metadata")
	}
	if m.Metadata["usage_completion_tokens"] == "" {
		t.Error("missing usage_completion_tokens in metadata")
	}
	if m.Metadata["usage_total_tokens"] == "" {
		t.Error("missing usage_total_tokens in metadata")
	}
	if m.Metadata["usage_prompt_tokens"] != "200" {
		t.Errorf("usage_prompt_tokens: want '200', got %q", m.Metadata["usage_prompt_tokens"])
	}
	if m.Metadata["usage_total_tokens"] != "280" {
		t.Errorf("usage_total_tokens: want '280', got %q", m.Metadata["usage_total_tokens"])
	}
}

// TestUsageMetadata_RunLoop_MetadataInOutboundMessage verifies Bug #1 fix:
// When the run() loop processes an inbound message, the outbound message contains usage metadata.
func TestUsageMetadata_RunLoop_MetadataInOutboundMessage(t *testing.T) {
	usage := &providers.UsageInfo{PromptTokens: 150, CompletionTokens: 60, TotalTokens: 210}
	provider := &usageMockProvider{response: "run loop response", usage: usage}
	al, msgBus, cleanup := newUsageTestAgentLoop(t, provider)
	defer cleanup()

	// Publish to inbound bus and run the agent loop in background
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		_ = al.Run(ctx)
	}()

	inMsg := bus.InboundMessage{
		Channel:    "test",
		SenderID:   "user1",
		ChatID:     "chat1",
		Content:    "hello from bus",
		SessionKey: "run-sess-1",
	}
	if err := msgBus.PublishInbound(ctx, inMsg); err != nil {
		t.Fatalf("PublishInbound: %v", err)
	}

	msgs := drainOutbound(msgBus, 1, 3*time.Second)
	cancel()
	<-runDone

	if len(msgs) == 0 {
		t.Fatal("expected 1 outbound message, got none")
	}

	m := msgs[0]
	if m.Metadata == nil {
		t.Fatal("expected Metadata to be non-nil in run() loop outbound message")
	}
	if m.Metadata["usage_prompt_tokens"] != "150" {
		t.Errorf("usage_prompt_tokens: want '150', got %q", m.Metadata["usage_prompt_tokens"])
	}
	if m.Metadata["usage_total_tokens"] != "210" {
		t.Errorf("usage_total_tokens: want '210', got %q", m.Metadata["usage_total_tokens"])
	}
}

// TestUsageMetadata_ErrorPath_ReturnsPartialUsage verifies Bug #2 fix:
// When the LLM succeeds on iteration 1 but fails on iteration 2, the returned
// usage contains the tokens from iteration 1.
func TestUsageMetadata_ErrorPath_ReturnsPartialUsage(t *testing.T) {
	callNum := 0
	partialUsage := &providers.UsageInfo{PromptTokens: 77, CompletionTokens: 33, TotalTokens: 110}

	provider := &callCountingProvider{
		chatFn: func(messages []providers.Message, toolDefs []providers.ToolDefinition) (*providers.LLMResponse, error) {
			callNum++
			if callNum == 1 {
				// First call: return a tool call so there's a second iteration
				return &providers.LLMResponse{
					Content: "",
					ToolCalls: []providers.ToolCall{
						{ID: "tc1", Name: "nonexistent_tool", Arguments: map[string]any{}},
					},
					Usage: partialUsage,
				}, nil
			}
			// Second call: hard LLM error (not a context error pattern, so no retry)
			return nil, fmt.Errorf("quota exceeded: rate limit")
		},
	}

	al, _, cleanup := newUsageTestAgentLoop(t, provider)
	defer cleanup()

	msg := bus.InboundMessage{
		Channel:    "test",
		SenderID:   "user1",
		ChatID:     "chat1",
		Content:    "do something",
		SessionKey: "err-sess-1",
	}
	_, gotUsage, err := al.processMessage(context.Background(), msg)
	// Error is expected on the second iteration
	if err == nil {
		t.Fatal("expected error from processMessage (LLM failed on iteration 2)")
	}
	if gotUsage == nil {
		t.Fatal("Bug #2: expected partial usage from first iteration, got nil")
	}
	if gotUsage.PromptTokens != 77 {
		t.Errorf("PromptTokens: want 77, got %d", gotUsage.PromptTokens)
	}
}

// callCountingProvider is a flexible mock that delegates to a function.
type callCountingProvider struct {
	chatFn func(messages []providers.Message, toolDefs []providers.ToolDefinition) (*providers.LLMResponse, error)
}

func (m *callCountingProvider) Chat(
	ctx context.Context,
	messages []providers.Message,
	toolDefs []providers.ToolDefinition,
	model string,
	opts map[string]any,
) (*providers.LLMResponse, error) {
	return m.chatFn(messages, toolDefs)
}

func (m *callCountingProvider) GetDefaultModel() string { return "mock-counting-model" }
