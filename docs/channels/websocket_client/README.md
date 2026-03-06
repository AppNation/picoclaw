# WebSocket Client Channel

The WebSocket client channel connects PicoClaw pods to a backend WebSocket server, enabling real-time bidirectional communication for multi-tenant Kubernetes pod architectures. Unlike other channels that receive inbound webhooks or poll APIs, this channel acts as a **client** that connects outward to your backend.

For the dedicated backend-driven skill install flow, see [`skill-install.md`](./skill-install.md).

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

| Type | Direction | Description |
|------|-----------|-------------|
| `"message"` | Backend → PicoClaw | A regular user message. Goes through allow-list checks, triggers the LLM, and sends a response back to the backend. |
| `"context"` | Backend → PicoClaw | A context-only message. Bypasses allow-list, triggers no LLM call, and sends no response. Content is injected into the agent's session history for use in future conversations. |
| `"command"` | Backend → PicoClaw | A pod-level operation (e.g. skill install). Bypasses LLM and bus. Produces lifecycle events. Requires `commands.enabled=true`. |
| `"response"` | PicoClaw → Backend | LLM response to a user message. |
| `"event"` | PicoClaw → Backend | Lifecycle event from a command execution. |

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

### Inbound — Command Message (Backend → PicoClaw)

Command messages trigger pod-level operations (e.g. skill installation) without involving the LLM. Commands are processed by dedicated handlers and produce lifecycle events back to the backend.

Commands are **disabled by default** and must be explicitly enabled via configuration.

```json
{
  "type": "command",
  "content": "{\"request_id\":\"req-abc-123\",\"slug\":\"docker-compose\",\"registry\":\"clawhub\",\"version\":\"1.2.0\",\"force\":false,\"timeout_sec\":60}",
  "metadata": {
    "command_name": "skill.install"
  }
}
```

| Field | Description |
|-------|-------------|
| `type` | Must be `"command"` |
| `content` | JSON-encoded command payload (see below) |
| `metadata.command_name` | Command to execute. Currently supported: `"skill.install"` |
| `metadata.request_id` | Fallback for `request_id` if not present in the JSON payload |

**`skill.install` payload fields:**

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `request_id` | string | yes | Idempotency key. Duplicate requests with the same ID are deduplicated |
| `slug` | string | yes | Skill identifier (e.g. `"docker-compose"`). Must not contain `/`, `\`, or `..` |
| `registry` | string | conditional | Registry name (e.g. `"clawhub"`). Required unless `default_registry` is configured |
| `version` | string | no | Specific version to install. Defaults to latest |
| `force` | bool | no | Force reinstall if already installed (default: `false`) |
| `timeout_sec` | int | no | Per-request timeout. Clamped to server-configured maximum |

**Behavior:**

- Commands bypass the LLM, allow-list, typing indicators, and message bus entirely
- Each command runs asynchronously in a bounded worker pool (`max_concurrent`)
- Duplicate `request_id` while a previous install is still running returns `accepted` with `already_in_progress=true`
- Duplicate `request_id` after completion (within `dedup_ttl_sec`) replays the terminal event with `replayed=true`
- If commands are disabled, the pod responds with `error_code: "commands_disabled"`

### Outbound — Command Events (PicoClaw → Backend)

Command lifecycle events use `type: "event"` and carry structured metadata:

```json
{
  "type": "event",
  "content": "skill install request accepted",
  "metadata": {
    "pod_hostname": "picoclaw-user-123",
    "timestamp": "2026-03-06T14:00:00Z",
    "event_name": "skill.install.accepted",
    "request_id": "req-abc-123",
    "slug": "docker-compose",
    "registry": "clawhub"
  }
}
```

**Lifecycle event sequence (happy path):**

```
skill.install.accepted  →  skill.install.started  →  skill.install.succeeded
```

**Lifecycle event sequence (failure):**

```
skill.install.accepted  →  skill.install.started  →  skill.install.failed
```

| Event Name | Description |
|------------|-------------|
| `skill.install.accepted` | Request validated and queued. May include `already_in_progress=true` for duplicate requests |
| `skill.install.started` | Worker acquired; download/install has begun |
| `skill.install.succeeded` | Skill installed successfully. Includes `duration_ms` |
| `skill.install.failed` | Installation failed. Includes `error_code`, `duration_ms` |

**Error codes:**

| Code | Description |
|------|-------------|
| `commands_disabled` | Commands feature is not enabled on this pod |
| `unsupported_command` | Unknown `command_name` |
| `invalid_command_payload` | Malformed JSON, missing `request_id`, or missing `slug` |
| `registry_required` | No registry specified and no `default_registry` configured |
| `registry_not_allowed` | Registry not in `allowed_registries` list |
| `invalid_slug` | Slug contains path traversal characters |
| `invalid_registry` | Registry name contains path traversal characters |
| `install_failed` | Download, extraction, or moderation check failed |
| `request_canceled` | Context canceled before install could start |
| `send_event_failed` | Failed to send the accepted event back to backend |

**Common metadata fields on all events:**

| Field | Description |
|-------|-------------|
| `pod_hostname` | Pod that processed this command |
| `timestamp` | RFC3339 timestamp |
| `event_name` | One of the lifecycle event names above |
| `request_id` | Idempotency key from the original command |
| `slug` | Skill slug |
| `registry` | Registry name |
| `duration_ms` | Total elapsed time (present on `succeeded` and `failed`) |

## Commands Configuration

### config.json

```json
{
  "channels": {
    "websocket_client": {
      "enabled": true,
      "backend_url": "wss://your-backend.com/ws",
      "commands": {
        "enabled": true,
        "max_concurrent": 1,
        "install_timeout_sec": 120,
        "dedup_ttl_sec": 300,
        "allowed_registries": ["clawhub"],
        "default_registry": "clawhub"
      }
    }
  }
}
```

### Commands Environment Variables

| Variable | Description |
|----------|-------------|
| `PICOCLAW_CHANNELS_WEBSOCKETCLIENT_COMMANDS_ENABLED` | Enable command processing (default: `false`) |
| `PICOCLAW_CHANNELS_WEBSOCKETCLIENT_COMMANDS_MAX_CONCURRENT` | Max parallel installs per pod (default: `1`) |
| `PICOCLAW_CHANNELS_WEBSOCKETCLIENT_COMMANDS_INSTALL_TIMEOUT_SEC` | Per-install timeout in seconds (default: `120`) |
| `PICOCLAW_CHANNELS_WEBSOCKETCLIENT_COMMANDS_DEDUP_TTL_SEC` | How long to cache completed results for replay (default: `300`) |
| `PICOCLAW_CHANNELS_WEBSOCKETCLIENT_COMMANDS_DEFAULT_REGISTRY` | Registry to use when command omits `registry` field |

### Commands Config Fields

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `enabled` | bool | no | Enable command processing. Default `false` (safe rollout) |
| `max_concurrent` | int | no | Maximum concurrent skill installs per pod (default: 1) |
| `install_timeout_sec` | int | no | Maximum seconds per install operation (default: 120) |
| `dedup_ttl_sec` | int | no | Seconds to retain completed results for idempotent replay (default: 300) |
| `allowed_registries` | array | no | If non-empty, only these registries are permitted. Empty = allow all configured registries |
| `default_registry` | string | no | Registry used when the command payload omits `registry` |

### Design Notes

- Commands use a separate code path from LLM-driven skill installation. Both paths reuse the same underlying install logic (validation, download, extraction, moderation) but have independent concurrency controls.
- The bounded worker pool (`max_concurrent`) prevents resource exhaustion on low-spec K8s pods.
- The dedup cache is in-memory per pod. If a pod restarts, the cache is lost and the backend should retry with a new `request_id` or the same one (which will re-execute since the cache is empty).

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
| Goroutines | 3 base (readLoop, pingLoop, reconnectLoop) + up to `max_concurrent` command workers |
| Memory | ~10–20 KB per connection |
| CPU | Near-zero when idle (event-driven reads) |
| WS buffers | 1 KB read + 1 KB write |

## Security

- Use `wss://` (TLS) in all non-local environments
- Set `auth_token` to authenticate pods to the backend
- Use `allow_from` to restrict which `sender_id` values are processed
- Validate `pod_hostname` format on the backend to prevent impersonation
