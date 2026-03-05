# Token Usage Metadata in WebSocket Outbound Messages

## Summary

Aggregate LLM token usage across all iterations of a single user turn and surface it as metadata fields (`usage_prompt_tokens`, `usage_completion_tokens`, `usage_total_tokens`) in the WebSocket outbound `"response"` message.

## Motivation

Backend infrastructure needs to track LLM token consumption per user turn for billing, rate-limiting, and observability. Token counts are already returned by every LLM provider in `LLMResponse.Usage` but are currently discarded after logging.

## Current State

- [`UsageInfo`](../pkg/providers/protocoltypes/types.go) — `{PromptTokens, CompletionTokens, TotalTokens int}` — fully defined
- [`LLMResponse.Usage *UsageInfo`](../pkg/providers/protocoltypes/types.go) — every LLM call already returns token counts
- [`OutboundWSMessage.Metadata`](../pkg/channels/websocket_client/websocket_client.go) — already carries `pod_hostname` + `timestamp`
- [`bus.OutboundMessage`](../pkg/bus/types.go) — **no Metadata field** — agent loop cannot attach token data
- Token usage discarded after each iteration in `runLLMIteration`

## Target Outbound Message

```json
{
  "type": "response",
  "user_id": "user123",
  "chat_id": "user123",
  "content": "Hello! How can I help?",
  "metadata": {
    "pod_hostname": "picoclaw-user-123",
    "timestamp": "2026-03-05T17:30:05Z",
    "usage_prompt_tokens": "1234",
    "usage_completion_tokens": "567",
    "usage_total_tokens": "1801"
  }
}
```

Token counts are **aggregated across all LLM iterations** for a single user turn.

## Data Flow

```
runLLMIteration  → accumulate UsageInfo per response → returns (string, int, *UsageInfo, error)
     ↓
runAgentLoop     → serialize usage to OutboundMessage.Metadata → returns (string, *UsageInfo, error)
     ↓
processMessage   → returns (string, *UsageInfo, error)
     ↓
main loop        → PublishOutbound(OutboundMessage{Metadata: usage_*})
     ↓
WS Send()        → merge msg.Metadata into OutboundWSMessage.Metadata (bus wins over channel defaults)
     ↓
Backend          → JSON with pod_hostname + timestamp + usage_*
```

## Files Changed

| File | Change |
|------|--------|
| `pkg/bus/types.go` | Add `Metadata map[string]string` to `OutboundMessage` |
| `pkg/agent/loop.go` | Accumulate `UsageInfo` in `runLLMIteration` (4th return); thread through `runAgentLoop` and `processMessage`; serialize to `OutboundMessage.Metadata` at both publish sites |
| `pkg/channels/websocket_client/websocket_client.go` | In `Send()`, merge `msg.Metadata` into `OutboundWSMessage.Metadata` |
| `pkg/channels/websocket_client/websocket_client_test.go` | Add `TestSendMessage_WithUsageMetadata` |
| `docs/channels/websocket_client/README.md` | Document new outbound metadata fields |

## Implementation Details

### 1. `bus.OutboundMessage`

```go
type OutboundMessage struct {
    Channel  string            `json:"channel"`
    ChatID   string            `json:"chat_id"`
    Content  string            `json:"content"`
    Metadata map[string]string `json:"metadata,omitempty"`
}
```

### 2. `runLLMIteration` — accumulate and return usage

```go
func (al *AgentLoop) runLLMIteration(...) (string, int, *providers.UsageInfo, error) {
    var totalUsage providers.UsageInfo
    hasUsage := false
    // ...inside loop after successful LLM call:
    if response.Usage != nil {
        totalUsage.PromptTokens     += response.Usage.PromptTokens
        totalUsage.CompletionTokens += response.Usage.CompletionTokens
        totalUsage.TotalTokens      += response.Usage.TotalTokens
        hasUsage = true
    }
    // ...
    if hasUsage {
        return finalContent, iteration, &totalUsage, nil
    }
    return finalContent, iteration, nil, nil
}
```

### 3. `usageToMetadata` helper

```go
func usageToMetadata(u *providers.UsageInfo) map[string]string {
    if u == nil {
        return nil
    }
    return map[string]string{
        "usage_prompt_tokens":     strconv.Itoa(u.PromptTokens),
        "usage_completion_tokens": strconv.Itoa(u.CompletionTokens),
        "usage_total_tokens":      strconv.Itoa(u.TotalTokens),
    }
}
```

### 4. WS `Send()` metadata merge

```go
outMeta := map[string]string{
    "pod_hostname": c.hostname,
    "timestamp":    time.Now().UTC().Format(time.RFC3339),
}
for k, v := range msg.Metadata {
    outMeta[k] = v  // bus metadata (usage_*) takes precedence
}
```

## Risks & Mitigations

| Risk | Mitigation |
|------|------------|
| Providers returning nil usage (Ollama, Claude CLI, CodexCLI) | `usageToMetadata(nil)` returns nil; merge loop is a no-op; outbound unchanged |
| Other channels ignore `OutboundMessage.Metadata` | Additive field; no existing `Send()` implementation reads it |
| `runAgentLoop` signature change | Two call sites in `processMessage` and `processSystemMessage`; both updated |
