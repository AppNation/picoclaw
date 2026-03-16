package agent

import (
	"context"
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

func newUsageTestAgentLoop(t *testing.T, provider providers.LLMProvider) (*AgentLoop, *bus.MessageBus, func()) {
	t.Helper()
	tmpDir, err := os.MkdirTemp("", "agent-usage-test-*")
	if err != nil {
		t.Fatalf("os.MkdirTemp: %v", err)
	}
	cfg := &config.Config{
		LLMEnabled: true,
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

// drainTokenUsage reads up to n token usage messages from the bus within timeout.
func drainTokenUsage(msgBus *bus.MessageBus, n int, timeout time.Duration) []bus.TokenUsageMessage {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var msgs []bus.TokenUsageMessage
	for i := 0; i < n; i++ {
		msg, ok := msgBus.SubscribeTokenUsage(ctx)
		if !ok {
			break
		}
		msgs = append(msgs, msg)
	}
	return msgs
}

// TestTokenUsage_ProcessDirectWithChannel_PublishesTokenUsage verifies that
// ProcessDirectWithChannel publishes a TokenUsageMessage through the bus.
func TestTokenUsage_ProcessDirectWithChannel_PublishesTokenUsage(t *testing.T) {
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

	msgs := drainTokenUsage(msgBus, 1, 2*time.Second)
	if len(msgs) == 0 {
		t.Fatal("expected 1 token usage message, got none")
	}

	m := msgs[0]
	if m.PromptTokens != 200 {
		t.Errorf("PromptTokens: want 200, got %d", m.PromptTokens)
	}
	if m.CompletionTokens != 80 {
		t.Errorf("CompletionTokens: want 80, got %d", m.CompletionTokens)
	}
	if m.TotalTokens != 280 {
		t.Errorf("TotalTokens: want 280, got %d", m.TotalTokens)
	}
	if m.ChatID != "user-abc" {
		t.Errorf("ChatID: want 'user-abc', got %q", m.ChatID)
	}
	if m.Channel != "websocket_client" {
		t.Errorf("Channel: want 'websocket_client', got %q", m.Channel)
	}
}

// TestTokenUsage_RunLoop_PublishesTokenUsage verifies that the run() loop
// publishes a TokenUsageMessage for each LLM call.
func TestTokenUsage_RunLoop_PublishesTokenUsage(t *testing.T) {
	usage := &providers.UsageInfo{PromptTokens: 150, CompletionTokens: 60, TotalTokens: 210}
	provider := &usageMockProvider{response: "run loop response", usage: usage}
	al, msgBus, cleanup := newUsageTestAgentLoop(t, provider)
	defer cleanup()

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

	msgs := drainTokenUsage(msgBus, 1, 3*time.Second)
	cancel()
	<-runDone

	if len(msgs) == 0 {
		t.Fatal("expected 1 token usage message, got none")
	}

	m := msgs[0]
	if m.PromptTokens != 150 {
		t.Errorf("PromptTokens: want 150, got %d", m.PromptTokens)
	}
	if m.TotalTokens != 210 {
		t.Errorf("TotalTokens: want 210, got %d", m.TotalTokens)
	}
}

// TestTokenUsage_ResponseHasNoUsageMetadata verifies that outbound response
// messages no longer carry usage metadata (moved to separate token_usage messages).
func TestTokenUsage_ResponseHasNoUsageMetadata(t *testing.T) {
	usage := &providers.UsageInfo{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150}
	provider := &usageMockProvider{response: "hello", usage: usage}
	al, msgBus, cleanup := newUsageTestAgentLoop(t, provider)
	defer cleanup()

	_, err := al.ProcessDirectWithChannel(
		context.Background(),
		"test",
		"sess-1",
		"websocket_client",
		"user-abc",
	)
	if err != nil {
		t.Fatalf("ProcessDirectWithChannel: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	outMsg, ok := msgBus.SubscribeOutbound(ctx)
	if !ok {
		t.Fatal("expected outbound message")
	}

	if outMsg.Metadata != nil {
		if _, exists := outMsg.Metadata["usage_prompt_tokens"]; exists {
			t.Error("response message should NOT contain usage_prompt_tokens metadata")
		}
	}
}
