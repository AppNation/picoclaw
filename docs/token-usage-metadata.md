# Token Usage Metadata

PicoClaw reports LLM token consumption as metadata on every outbound WebSocket response. This allows the backend to track per-user, per-turn costs accurately.

---

## Metadata Keys

Every response message carries the following fields in its `metadata` map:

| Key | Type | Description |
|---|---|---|
| `usage_prompt_tokens` | string (integer) | Input tokens consumed in this turn |
| `usage_completion_tokens` | string (integer) | Output tokens generated in this turn |
| `usage_total_tokens` | string (integer) | Total tokens (prompt + completion) |

These are **cumulative for the entire turn** — all LLM iterations, all tool-call roundtrips, and all synchronous subagent calls are included in the final count.

### Example WebSocket response

```json
{
  "type": "response",
  "content": "Here is your analysis...",
  "metadata": {
    "pod_hostname": "picoclaw-abc123",
    "timestamp": "2026-03-12T13:09:36Z",
    "usage_prompt_tokens": "4821",
    "usage_completion_tokens": "1243",
    "usage_total_tokens": "6064"
  }
}
```

---

## What Is Counted

The token count in a response covers everything the agent consumed to produce that response:

| Source | Counted? |
|---|---|
| Direct LLM answer (single iteration) | ✅ Yes |
| Multi-iteration tool-call loops | ✅ Yes — all iterations accumulated |
| `message` tool content (mid-turn publish) | ✅ Yes — tokens included in final confirmation message |
| Synchronous subagent (`subagent` tool) | ✅ Yes — subagent tokens merged into parent total |
| LLM error after partial success | ✅ Yes — partial tokens from completed iterations are returned |
| Cron agent jobs (`deliver=false`) | ✅ Yes — published via bus with metadata |

---

## What Is NOT Counted (Hidden Cost)

Some LLM calls are made outside the user message turn and cannot be attached to a response:

| Source | Why not counted |
|---|---|
| **Heartbeat** | Fires every 30 minutes per pod independently of any user message. No user turn to attach to. |
| **Summarization** | Runs asynchronously after the user response has already been sent. Cannot retroactively update metadata. |
| **Cron deliver=true** | No LLM call — sends a fixed string directly. Zero token cost. |
| **Cron shell commands** | No LLM call — executes a shell command. Zero token cost. |

**Key note:** `HEARTBEAT.md` is created automatically by picoclaw on first run. Even with no user-configured tasks, the default template triggers real LLM calls every 30 minutes per pod.

---

## Message Tool Behavior

When the LLM uses the `message` tool to send content mid-turn, the user receives **two messages**:

1. **Tool-sent message** — the actual content (e.g. a stock analysis). No metadata.
2. **Final confirmation** — a short LLM acknowledgment (e.g. "I've sent the analysis"). Contains full cumulative usage metadata for the entire turn.

This is by design: token counts are only fully known after all LLM iterations complete. The metadata on the second message is the authoritative count for the entire turn.

---

## Implementation

Token metadata flows through the following path:

```
Provider.Chat() → LLMResponse.Usage
  ↓
runLLMIteration() → accumulates totalUsage across all iterations
  ↓
runAgentLoop() → returns (content, usage, err)
  ↓
run() loop → PublishOutbound with usageToMetadata(usage)
ProcessDirectWithChannel() → PublishOutbound with usageToMetadata(usage)
  ↓
MessageBus → WebSocketClientChannel.Send()
  ↓
WebSocket JSON → metadata map
```

For subagent tool calls, usage propagates up:

```
RunToolLoop() → ToolLoopResult.Usage
  ↓
SubagentTool.Execute() → ToolResult.Usage
  ↓
runLLMIteration() → accumulated into parent totalUsage
```