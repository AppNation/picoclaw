# WebSocket Client Channel

The WebSocket client channel connects PicoClaw pods to a backend WebSocket server, enabling real-time bidirectional communication for multi-tenant Kubernetes pod architectures. Unlike other channels that receive inbound webhooks or poll APIs, this channel acts as a **client** that connects outward to your backend.

## Architecture

```
User App ──► Backend API ──► Redis Pub/Sub ──► Backend WS Server ──► PicoClaw Pod
PicoClaw Pod ──► Backend WS Server ──► PostgreSQL ──► Push Notification ──► User App
```

Each PicoClaw pod identifies itself to the backend using its Kubernetes `HOSTNAME` (automatically set to the pod name via the downward API or `metadata.name`).

## Configuration

### config.json

```json
{
  "channels": {
    "websocket_client": {
      "enabled": true,
      "backend_url": "wss://your-backend.com/ws",
      "auth_token": "your-secret-token",
      "reconnect_delay": 5,
      "ping_interval": 30,
      "allow_from": []
    }
  }
}
```

### Environment Variables

| Variable | Description |
|----------|-------------|
| `PICOCLAW_CHANNELS_WEBSOCKETCLIENT_ENABLED` | Enable/disable the channel |
| `PICOCLAW_CHANNELS_WEBSOCKETCLIENT_BACKEND_URL` | WebSocket server URL (e.g. `wss://backend.internal/ws`) |
| `PICOCLAW_CHANNELS_WEBSOCKETCLIENT_AUTH_TOKEN` | Optional bearer token for authentication |
| `PICOCLAW_CHANNELS_WEBSOCKETCLIENT_RECONNECT_DELAY` | Base reconnection delay in seconds (default: 5) |
| `PICOCLAW_CHANNELS_WEBSOCKETCLIENT_PING_INTERVAL` | Keepalive ping interval in seconds (default: 30) |
| `PICOCLAW_CHANNELS_WEBSOCKETCLIENT_ALLOW_FROM` | Comma-separated sender ID whitelist (e.g., `user1,user2`; empty = allow all) |

### Config Fields

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `enabled` | bool | yes | Enable the channel |
| `backend_url` | string | yes | WebSocket server URL. Use `wss://` in production |
| `auth_token` | string | no | Sent as `Authorization: Bearer <token>` during handshake |
| `reconnect_delay` | int | no | Base delay between reconnection attempts (seconds, default: 5) |
| `ping_interval` | int | no | How often to send keepalive pings (seconds, default: 30) |
| `allow_from` | array | no | Sender ID whitelist. Empty array allows all senders |

## Kubernetes Deployment

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: picoclaw-user-123
spec:
  template:
    spec:
      containers:
      - name: picoclaw
        env:
        - name: HOSTNAME
          valueFrom:
            fieldRef:
              fieldPath: metadata.name
        - name: PICOCLAW_CHANNELS_WEBSOCKETCLIENT_ENABLED
          value: "true"
        - name: PICOCLAW_CHANNELS_WEBSOCKETCLIENT_BACKEND_URL
          value: "wss://backend.default.svc.cluster.local:3000/ws"
        - name: PICOCLAW_CHANNELS_WEBSOCKETCLIENT_AUTH_TOKEN
          valueFrom:
            secretKeyRef:
              name: picoclaw-secrets
              key: ws-auth-token
```

The `HOSTNAME` environment variable is automatically set by Kubernetes to the pod name when using the downward API field `metadata.name`. The channel appends it as a query parameter: `wss://backend/ws?pod_hostname=picoclaw-user-123`.

## Message Protocol

### Message Types

| Type | Description |
|------|-------------|
| `"message"` | A regular user message. Goes through allow-list checks, triggers the LLM, and sends a response back to the backend. |
| `"context"` | A context-only message. Bypasses allow-list, triggers no LLM call, and sends no response. Content is injected into the agent's session history for use in future conversations. |

### Inbound — Regular Message (Backend → PicoClaw)

```json
{
  "type": "message",
  "user_id": "user123",
  "chat_id": "user123",
  "sender_id": "user123",
  "content": "Hello PicoClaw!",
  "metadata": {
    "source": "mobile_app",
    "timestamp": "2026-02-16T10:30:00Z"
  }
}
```

| Field | Description |
|-------|-------------|
| `type` | `"message"` for regular user messages |
| `user_id` | User identifier |
| `chat_id` | Chat/session identifier. Defaults to `user_id` if empty |
| `sender_id` | Sender identifier for allow-list checks. Defaults to `user_id` if empty |
| `content` | Message text content |
| `metadata` | Optional key-value metadata passed through to the agent |

### Inbound — Context Message (Backend → PicoClaw)

Context messages inject backend events into the agent's session history without triggering an LLM call or sending any reply. The agent silently gains awareness of the event and uses it when answering the next real user message.

```json
{
  "type": "context",
  "user_id": "user123",
  "chat_id": "user123",
  "content": "User upgraded to premium tier"
}
```

| Field | Description |
|-------|-------------|
| `type` | Must be `"context"` |
| `user_id` | User identifier (used to resolve the session) |
| `chat_id` | Chat/session identifier. Defaults to `user_id` if empty |
| `sender_id` | Optional. Identifies the source (e.g. `"billing-service"`). Not used for allow-list checks |
| `content` | Context text to inject (required; empty content is silently dropped) |
| `metadata` | Optional key-value metadata |

**Behavior:**

- `allow_from` is **not** applied — context messages come from trusted backend infrastructure, not users. Because PicoClaw unconditionally trusts `type: "context"` senders, **the backend must enforce service-to-service authentication and validate the source of every context message**. Recommended controls (defense-in-depth):
  - **mTLS or token-based auth** — require mutual TLS or a signed bearer token between your service and PicoClaw
  - **Message signing/verification** — HMAC-sign the payload and verify before forwarding to PicoClaw
  - **IP allowlisting** — restrict which IPs are permitted to send context messages
  - **Audit logging** — log every context message with its source identity and timestamp
  - **Rotate credentials regularly** — cycle mTLS certificates and auth tokens on a schedule
  - **Validate message provenance** — reject context messages whose source cannot be verified against your service registry
- No typing indicator, reaction, or placeholder is triggered
- No response is sent back to the backend
- The content is stored in the agent session as `[Context] <content>` and becomes part of the LLM context on the next real user message

**Example flow:**

```
14:00  Backend sends:  type=context, content="User upgraded to premium tier"
       PicoClaw:       Saved to session history. No response sent.

14:05  User sends:     "What features do I have access to now?"
       PicoClaw LLM sees:
           [Context] User upgraded to premium tier     ← injected at 14:00
           User: What features do I have access to now?
       PicoClaw responds with awareness of the upgrade.
```

### Outbound (PicoClaw → Backend)

```json
{
  "type": "response",
  "user_id": "user123",
  "chat_id": "user123",
  "content": "Hello! How can I help you?",
  "metadata": {
    "pod_hostname": "picoclaw-user-123",
    "timestamp": "2026-02-16T10:30:05Z",
    "usage_prompt_tokens": "1234",
    "usage_completion_tokens": "567",
    "usage_total_tokens": "1801"
  }
}
```

| Metadata field | Description |
|----------------|-------------|
| `pod_hostname` | Pod that generated this response (set automatically) |
| `timestamp` | RFC3339 timestamp when the response was sent (set automatically) |
| `usage_prompt_tokens` | Total prompt tokens consumed across all LLM iterations for this turn (omitted when the provider does not report usage, e.g. Ollama or local CLI providers) |
| `usage_completion_tokens` | Total completion tokens generated across all LLM iterations for this turn |
| `usage_total_tokens` | Sum of prompt and completion tokens |

Token counts are **aggregated across all LLM iterations** within a single user turn. A turn that requires multiple tool calls (and therefore multiple LLM calls) reports the total tokens for the entire turn, not per-call.

## Connection Lifecycle

### Startup

1. Pod reads `HOSTNAME` env var (set by Kubernetes)
2. Channel connects to `backend_url?pod_hostname=<hostname>`
3. If `auth_token` is set, sends `Authorization: Bearer <token>` header
4. Starts `readLoop`, `pingLoop`, and `reconnectLoop` goroutines

### Reconnection

On connection loss, the `reconnectLoop` retries with exponential backoff:

| Attempt | Delay |
|---------|-------|
| 1 | `reconnect_delay` s + jitter |
| 2 | `reconnect_delay × 2` s + jitter |
| 3 | `reconnect_delay × 4` s + jitter |
| … | … |
| cap | 60 s + jitter |

Random jitter (0–1 s) is added to each delay to prevent thundering herd when many pods reconnect simultaneously after a backend restart.

### Keepalive

The `pingLoop` sends a WebSocket ping frame every `ping_interval` seconds. The backend's automatic pong response resets the read deadline. If no pong is received within `2 × ping_interval` seconds, the connection is considered dead and triggers reconnection.

### Graceful Shutdown

On `Stop()`, the channel sends a WebSocket close frame (`1000 Normal Closure`) before closing the connection.

## Backend Implementation Notes

The backend must:

1. Expose a WebSocket endpoint (e.g. `/ws`)
2. Read `pod_hostname` from the query string to identify the pod
3. Optionally validate the `Authorization: Bearer` token
4. Route messages to the correct pod via the `pod_hostname`
5. For multi-instance backends, use Redis pub/sub to route across instances

Example connection URL your backend will receive:
```
wss://backend.internal/ws?pod_hostname=picoclaw-user-123
```

## Resource Usage (Low-Spec Pods)

Designed for minimal resource footprint:

| Resource | Per Pod |
|----------|---------|
| Goroutines | 3 (readLoop, pingLoop, reconnectLoop) |
| Memory | ~10–20 KB per connection |
| CPU | Near-zero when idle (event-driven reads) |
| WS buffers | 1 KB read + 1 KB write |

## Security

- Use `wss://` (TLS) in all non-local environments
- Set `auth_token` to authenticate pods to the backend
- Use `allow_from` to restrict which `sender_id` values are processed
- Validate `pod_hostname` format on the backend to prevent impersonation
