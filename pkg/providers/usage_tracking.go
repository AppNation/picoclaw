package providers

import (
	"context"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/logger"
)

type trackingContextKey int

const (
	ctxKeyChatID  trackingContextKey = iota
	ctxKeyChannel
)

// WithLLMTrackingContext attaches chat/channel info to the context so the
// UsageTrackingProvider can attribute usage to the right conversation.
func WithLLMTrackingContext(ctx context.Context, chatID, channel string) context.Context {
	ctx = context.WithValue(ctx, ctxKeyChatID, chatID)
	ctx = context.WithValue(ctx, ctxKeyChannel, channel)
	return ctx
}

func trackingChatID(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyChatID).(string)
	return v
}

func trackingChannel(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyChannel).(string)
	return v
}

// UsageTrackingProvider wraps any LLMProvider and reports token usage for
// every single Chat() call through the message bus. This is the single
// interception point that guarantees 100% cost coverage.
type UsageTrackingProvider struct {
	delegate LLMProvider
	bus      *bus.MessageBus
}

// NewUsageTrackingProvider wraps a provider so that every Chat() call
// publishes a TokenUsageMessage via the bus.
func NewUsageTrackingProvider(delegate LLMProvider, msgBus *bus.MessageBus) *UsageTrackingProvider {
	return &UsageTrackingProvider{
		delegate: delegate,
		bus:      msgBus,
	}
}

func (p *UsageTrackingProvider) Chat(
	ctx context.Context,
	messages []Message,
	tools []ToolDefinition,
	model string,
	options map[string]any,
) (*LLMResponse, error) {
	resp, err := p.delegate.Chat(ctx, messages, tools, model, options)

	// Report usage regardless of error — partial responses can still carry usage.
	if resp != nil && resp.Usage != nil {
		p.reportUsage(ctx, model, resp.Usage)
	}

	return resp, err
}

func (p *UsageTrackingProvider) GetDefaultModel() string {
	return p.delegate.GetDefaultModel()
}

func (p *UsageTrackingProvider) reportUsage(ctx context.Context, model string, usage *UsageInfo) {
	if usage.TotalTokens == 0 && usage.PromptTokens == 0 && usage.CompletionTokens == 0 {
		return
	}

	channel := trackingChannel(ctx)
	if channel == "" {
		channel = "unknown"
	}

	p.bus.PublishTokenUsage(ctx, bus.TokenUsageMessage{
		Channel:          channel,
		ChatID:           trackingChatID(ctx),
		Model:            model,
		PromptTokens:     usage.PromptTokens,
		CompletionTokens: usage.CompletionTokens,
		TotalTokens:      usage.TotalTokens,
	})

	logger.DebugCF("usage", "Token usage reported", map[string]any{
		"model":             model,
		"prompt_tokens":     usage.PromptTokens,
		"completion_tokens": usage.CompletionTokens,
		"total_tokens":      usage.TotalTokens,
		"channel":           channel,
	})
}

// Unwrap returns the underlying provider (useful for type assertions like StatefulProvider).
func (p *UsageTrackingProvider) Unwrap() LLMProvider {
	return p.delegate
}
