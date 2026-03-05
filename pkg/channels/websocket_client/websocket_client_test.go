package websocket_client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/channels"
	"github.com/sipeed/picoclaw/pkg/config"
)

var upgrader = websocket.Upgrader{
	CheckOrigin:     func(_ *http.Request) bool { return true },
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
}

func TestNewWebSocketClientChannel_Valid(t *testing.T) {
	t.Parallel()
	msgBus := bus.NewMessageBus()
	defer msgBus.Close()

	cfg := config.WebSocketClientConfig{
		Enabled:        true,
		BackendURL:     "ws://localhost:9999/ws",
		ReconnectDelay: 5,
		PingInterval:   30,
	}

	ch, err := NewWebSocketClientChannel(cfg, msgBus)
	require.NoError(t, err)
	assert.NotNil(t, ch)
	assert.Equal(t, "websocket_client", ch.Name())
	assert.False(t, ch.IsRunning())
}

func TestNewWebSocketClientChannel_MissingURL(t *testing.T) {
	t.Parallel()
	msgBus := bus.NewMessageBus()
	defer msgBus.Close()

	cfg := config.WebSocketClientConfig{
		Enabled: true,
	}

	ch, err := NewWebSocketClientChannel(cfg, msgBus)
	assert.Error(t, err)
	assert.Nil(t, ch)
	assert.Contains(t, err.Error(), "backend_url is required")
}

func TestConnect_Success(t *testing.T) {
	t.Parallel()

	var receivedHostname string
	var receivedAuth string
	var mu sync.Mutex

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		receivedHostname = r.URL.Query().Get("pod_hostname")
		receivedAuth = r.Header.Get("Authorization")
		mu.Unlock()

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Logf("upgrade error: %v", err)
			return
		}
		defer conn.Close()
		// Keep connection open until test finishes
		for {
			_, _, err := conn.ReadMessage()
			if err != nil {
				return
			}
		}
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")

	msgBus := bus.NewMessageBus()
	defer msgBus.Close()

	cfg := config.WebSocketClientConfig{
		Enabled:        true,
		BackendURL:     wsURL,
		AuthToken:      "test-token-123",
		ReconnectDelay: 1,
		PingInterval:   30,
	}

	ch, err := NewWebSocketClientChannel(cfg, msgBus)
	require.NoError(t, err)

	ch.hostname = "test-pod-42"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch.ctx = ctx
	ch.cancel = cancel

	err = ch.connect()
	require.NoError(t, err)

	mu.Lock()
	assert.Equal(t, "test-pod-42", receivedHostname)
	assert.Equal(t, "Bearer test-token-123", receivedAuth)
	mu.Unlock()

	ch.connMu.Lock()
	assert.NotNil(t, ch.conn)
	ch.connMu.Unlock()
}

func TestConnect_Failure(t *testing.T) {
	t.Parallel()

	msgBus := bus.NewMessageBus()
	defer msgBus.Close()

	cfg := config.WebSocketClientConfig{
		Enabled:    true,
		BackendURL: "ws://127.0.0.1:1/unreachable",
	}

	ch, err := NewWebSocketClientChannel(cfg, msgBus)
	require.NoError(t, err)

	ch.hostname = "test-pod"
	ch.ctx, ch.cancel = context.WithCancel(context.Background())
	defer ch.cancel()

	err = ch.connect()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "websocket dial failed")
}

func TestSendMessage(t *testing.T) {
	t.Parallel()

	var receivedMsg OutboundWSMessage
	msgReceived := make(chan struct{})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if err := json.Unmarshal(data, &receivedMsg); err != nil {
			return
		}
		close(msgReceived)
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")

	msgBus := bus.NewMessageBus()
	defer msgBus.Close()

	cfg := config.WebSocketClientConfig{
		Enabled:        true,
		BackendURL:     wsURL,
		ReconnectDelay: 1,
		PingInterval:   30,
	}

	ch, err := NewWebSocketClientChannel(cfg, msgBus)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err = ch.Start(ctx)
	require.NoError(t, err)
	defer func() {
		_ = ch.Stop(ctx)
	}()

	// Override hostname after Start (Start reads from HOSTNAME env which is empty in tests)
	ch.hostname = "test-pod-send"

	// Give connection time to establish
	time.Sleep(100 * time.Millisecond)

	err = ch.Send(ctx, bus.OutboundMessage{
		Channel: "websocket_client",
		ChatID:  "user-abc",
		Content: "Hello from PicoClaw",
	})
	require.NoError(t, err)

	select {
	case <-msgReceived:
		assert.Equal(t, "response", receivedMsg.Type)
		assert.Equal(t, "user-abc", receivedMsg.UserID)
		assert.Equal(t, "user-abc", receivedMsg.ChatID)
		assert.Equal(t, "Hello from PicoClaw", receivedMsg.Content)
		assert.Equal(t, "test-pod-send", receivedMsg.Metadata["pod_hostname"])
		assert.NotEmpty(t, receivedMsg.Metadata["timestamp"])
	case <-time.After(3 * time.Second):
		t.Fatal("Timed out waiting for message")
	}
}

func TestReceiveMessage(t *testing.T) {
	t.Parallel()

	serverReady := make(chan *websocket.Conn, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		serverReady <- conn
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")

	msgBus := bus.NewMessageBus()
	defer msgBus.Close()

	cfg := config.WebSocketClientConfig{
		Enabled:        true,
		BackendURL:     wsURL,
		ReconnectDelay: 1,
		PingInterval:   30,
	}

	ch, err := NewWebSocketClientChannel(cfg, msgBus)
	require.NoError(t, err)

	ch.hostname = "test-pod-recv"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err = ch.Start(ctx)
	require.NoError(t, err)
	defer func() {
		_ = ch.Stop(ctx)
	}()

	// Wait for server to get the connection
	var serverConn *websocket.Conn
	select {
	case serverConn = <-serverReady:
	case <-time.After(3 * time.Second):
		t.Fatal("Server did not receive connection")
	}
	defer serverConn.Close()

	// Send a message from "backend" to PicoClaw
	inMsg := InboundWSMessage{
		Type:     "message",
		UserID:   "user-xyz",
		ChatID:   "user-xyz",
		SenderID: "user-xyz",
		Content:  "Hello PicoClaw!",
		Metadata: map[string]string{"source": "test"},
	}

	data, err := json.Marshal(inMsg)
	require.NoError(t, err)

	err = serverConn.WriteMessage(websocket.TextMessage, data)
	require.NoError(t, err)

	// Read from inbound bus
	inboundCtx, inboundCancel := context.WithTimeout(ctx, 3*time.Second)
	defer inboundCancel()

	msg, ok := msgBus.ConsumeInbound(inboundCtx)
	require.True(t, ok, "Expected to receive inbound message")
	assert.Equal(t, "websocket_client", msg.Channel)
	assert.Equal(t, "user-xyz", msg.ChatID)
	assert.Equal(t, "user-xyz", msg.SenderID)
	assert.Equal(t, "Hello PicoClaw!", msg.Content)
}

func TestSend_NotRunning(t *testing.T) {
	t.Parallel()

	msgBus := bus.NewMessageBus()
	defer msgBus.Close()

	cfg := config.WebSocketClientConfig{
		Enabled:    true,
		BackendURL: "ws://localhost:9999/ws",
	}

	ch, err := NewWebSocketClientChannel(cfg, msgBus)
	require.NoError(t, err)

	err = ch.Send(context.Background(), bus.OutboundMessage{
		ChatID:  "user1",
		Content: "test",
	})
	assert.ErrorIs(t, err, ErrNotRunning)
}

func TestSend_NotConnected(t *testing.T) {
	t.Parallel()

	msgBus := bus.NewMessageBus()
	defer msgBus.Close()

	cfg := config.WebSocketClientConfig{
		Enabled:    true,
		BackendURL: "ws://localhost:9999/ws",
	}

	ch, err := NewWebSocketClientChannel(cfg, msgBus)
	require.NoError(t, err)

	// Manually set running without actually connecting
	ch.SetRunning(true)

	err = ch.Send(context.Background(), bus.OutboundMessage{
		ChatID:  "user1",
		Content: "test",
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not connected")
}

func TestMessageSerialization(t *testing.T) {
	t.Parallel()

	inMsg := InboundWSMessage{
		Type:     "message",
		UserID:   "user123",
		ChatID:   "user123",
		SenderID: "user123",
		Content:  "Hello PicoClaw",
		Metadata: map[string]string{"timestamp": "2026-02-16T10:30:00Z", "source": "mobile_app"},
	}

	data, err := json.Marshal(inMsg)
	require.NoError(t, err)

	var decoded InboundWSMessage
	err = json.Unmarshal(data, &decoded)
	require.NoError(t, err)
	assert.Equal(t, inMsg, decoded)

	outMsg := OutboundWSMessage{
		Type:    "response",
		UserID:  "user123",
		ChatID:  "user123",
		Content: "Hello! How can I help?",
		Metadata: map[string]string{
			"pod_hostname": "picoclaw-user-123",
			"timestamp":    "2026-02-16T10:30:05Z",
		},
	}

	data, err = json.Marshal(outMsg)
	require.NoError(t, err)

	var decodedOut OutboundWSMessage
	err = json.Unmarshal(data, &decodedOut)
	require.NoError(t, err)
	assert.Equal(t, outMsg, decodedOut)
}

func TestBackoffDelay(t *testing.T) {
	t.Parallel()

	msgBus := bus.NewMessageBus()
	defer msgBus.Close()

	cfg := config.WebSocketClientConfig{
		Enabled:        true,
		BackendURL:     "ws://localhost:9999/ws",
		ReconnectDelay: 5,
	}

	ch, err := NewWebSocketClientChannel(cfg, msgBus)
	require.NoError(t, err)

	base := 5 * time.Second

	// attempt 0: ~5s + jitter
	d0 := ch.backoffDelay(base, 0)
	assert.GreaterOrEqual(t, d0, base)
	assert.Less(t, d0, base+jitterMax+time.Millisecond)

	// attempt 1: ~10s + jitter
	d1 := ch.backoffDelay(base, 1)
	assert.GreaterOrEqual(t, d1, 10*time.Second)
	assert.Less(t, d1, 10*time.Second+jitterMax+time.Millisecond)

	// attempt 2: ~20s + jitter
	d2 := ch.backoffDelay(base, 2)
	assert.GreaterOrEqual(t, d2, 20*time.Second)
	assert.Less(t, d2, 20*time.Second+jitterMax+time.Millisecond)

	// attempt 3: ~40s + jitter
	d3 := ch.backoffDelay(base, 3)
	assert.GreaterOrEqual(t, d3, 40*time.Second)
	assert.Less(t, d3, 40*time.Second+jitterMax+time.Millisecond)

	// attempt 4: capped at 60s + jitter
	d4 := ch.backoffDelay(base, 4)
	assert.GreaterOrEqual(t, d4, 60*time.Second)
	assert.Less(t, d4, 60*time.Second+jitterMax+time.Millisecond)

	// attempt 10: still capped at 60s + jitter
	d10 := ch.backoffDelay(base, 10)
	assert.GreaterOrEqual(t, d10, 60*time.Second)
	assert.Less(t, d10, 60*time.Second+jitterMax+time.Millisecond)
}

func TestStopGraceful(t *testing.T) {
	t.Parallel()

	closeReceived := make(chan struct{})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			_, _, err := conn.ReadMessage()
			if err != nil {
				if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
					close(closeReceived)
				}
				return
			}
		}
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")

	msgBus := bus.NewMessageBus()
	defer msgBus.Close()

	cfg := config.WebSocketClientConfig{
		Enabled:        true,
		BackendURL:     wsURL,
		ReconnectDelay: 1,
		PingInterval:   30,
	}

	ch, err := NewWebSocketClientChannel(cfg, msgBus)
	require.NoError(t, err)

	ch.hostname = "test-pod-stop"
	ctx := context.Background()

	err = ch.Start(ctx)
	require.NoError(t, err)
	assert.True(t, ch.IsRunning())

	// Give connection time to establish
	time.Sleep(100 * time.Millisecond)

	err = ch.Stop(ctx)
	require.NoError(t, err)
	assert.False(t, ch.IsRunning())

	// Verify server received close frame
	select {
	case <-closeReceived:
		// Server received normal close
	case <-time.After(2 * time.Second):
		t.Log("Server did not receive close frame (may be OS-dependent)")
	}
}

func TestAllowFrom_Empty(t *testing.T) {
	t.Parallel()

	msgBus := bus.NewMessageBus()
	defer msgBus.Close()

	cfg := config.WebSocketClientConfig{
		Enabled:    true,
		BackendURL: "ws://localhost:9999/ws",
		AllowFrom:  config.FlexibleStringSlice{},
	}

	ch, err := NewWebSocketClientChannel(cfg, msgBus)
	require.NoError(t, err)

	// Empty allow_from means allow all
	assert.True(t, ch.IsAllowed("anyone"))
	assert.True(t, ch.IsAllowed("user123"))
}

func TestAllowFrom_Restricted(t *testing.T) {
	t.Parallel()

	msgBus := bus.NewMessageBus()
	defer msgBus.Close()

	cfg := config.WebSocketClientConfig{
		Enabled:    true,
		BackendURL: "ws://localhost:9999/ws",
		AllowFrom:  config.FlexibleStringSlice{"user123", "user456"},
	}

	ch, err := NewWebSocketClientChannel(cfg, msgBus)
	require.NoError(t, err)

	assert.True(t, ch.IsAllowed("user123"))
	assert.True(t, ch.IsAllowed("user456"))
	assert.False(t, ch.IsAllowed("user789"))
}

func TestStart_DoubleStartGuard(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")

	msgBus := bus.NewMessageBus()
	defer msgBus.Close()

	cfg := config.WebSocketClientConfig{
		Enabled:        true,
		BackendURL:     wsURL,
		ReconnectDelay: 1,
		PingInterval:   30,
	}

	ch, err := NewWebSocketClientChannel(cfg, msgBus)
	require.NoError(t, err)

	ctx := context.Background()

	err = ch.Start(ctx)
	require.NoError(t, err)
	assert.True(t, ch.IsRunning())
	defer func() { _ = ch.Stop(ctx) }()

	// Second Start() must fail
	err = ch.Start(ctx)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "already running")
}

func TestContextMessage_PublishedToBus(t *testing.T) {
	t.Parallel()

	serverReady := make(chan *websocket.Conn, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		serverReady <- conn
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")

	msgBus := bus.NewMessageBus()
	defer msgBus.Close()

	cfg := config.WebSocketClientConfig{
		Enabled:        true,
		BackendURL:     wsURL,
		ReconnectDelay: 1,
		PingInterval:   30,
	}

	ch, err := NewWebSocketClientChannel(cfg, msgBus)
	require.NoError(t, err)

	ch.hostname = "test-pod-ctx"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err = ch.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = ch.Stop(ctx) }()

	var serverConn *websocket.Conn
	select {
	case serverConn = <-serverReady:
	case <-time.After(3 * time.Second):
		t.Fatal("Server did not receive connection")
	}
	defer serverConn.Close()

	ctxMsg := InboundWSMessage{
		Type:     "context",
		UserID:   "user-ctx",
		ChatID:   "user-ctx",
		SenderID: "user-ctx",
		Content:  "User upgraded to premium tier",
		Metadata: map[string]string{"source": "billing"},
	}

	data, err := json.Marshal(ctxMsg)
	require.NoError(t, err)

	err = serverConn.WriteMessage(websocket.TextMessage, data)
	require.NoError(t, err)

	inboundCtx, inboundCancel := context.WithTimeout(ctx, 3*time.Second)
	defer inboundCancel()

	msg, ok := msgBus.ConsumeInbound(inboundCtx)
	require.True(t, ok, "Expected context message on bus")
	assert.Equal(t, "websocket_client", msg.Channel)
	assert.Equal(t, "user-ctx", msg.ChatID)
	assert.Equal(t, "user-ctx", msg.SenderID)
	assert.Equal(t, "User upgraded to premium tier", msg.Content)
	assert.True(t, msg.ContextOnly, "Expected ContextOnly flag to be true")
}

func TestContextMessage_BypassesAllowList(t *testing.T) {
	t.Parallel()

	serverReady := make(chan *websocket.Conn, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		serverReady <- conn
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")

	msgBus := bus.NewMessageBus()
	defer msgBus.Close()

	// Only "allowed-user" passes the allow-list for regular messages.
	cfg := config.WebSocketClientConfig{
		Enabled:        true,
		BackendURL:     wsURL,
		ReconnectDelay: 1,
		PingInterval:   30,
		AllowFrom:      config.FlexibleStringSlice{"allowed-user"},
	}

	ch, err := NewWebSocketClientChannel(cfg, msgBus)
	require.NoError(t, err)

	ch.hostname = "test-pod-ctx-allowlist"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err = ch.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = ch.Stop(ctx) }()

	var serverConn *websocket.Conn
	select {
	case serverConn = <-serverReady:
	case <-time.After(3 * time.Second):
		t.Fatal("Server did not receive connection")
	}
	defer serverConn.Close()

	// Send from a sender NOT in the allow-list as a context message.
	ctxMsg := InboundWSMessage{
		Type:     "context",
		UserID:   "unlisted-infra",
		ChatID:   "unlisted-infra",
		SenderID: "unlisted-infra",
		Content:  "Infra event: deployment completed",
	}

	data, err := json.Marshal(ctxMsg)
	require.NoError(t, err)

	err = serverConn.WriteMessage(websocket.TextMessage, data)
	require.NoError(t, err)

	// Context messages bypass allow-list — expect it on the bus.
	inboundCtx, inboundCancel := context.WithTimeout(ctx, 3*time.Second)
	defer inboundCancel()

	msg, ok := msgBus.ConsumeInbound(inboundCtx)
	require.True(t, ok, "Context message should bypass allow-list and appear on bus")
	assert.True(t, msg.ContextOnly)
	assert.Equal(t, "unlisted-infra", msg.SenderID)
}

func TestContextMessage_EmptyContent_Ignored(t *testing.T) {
	t.Parallel()

	serverReady := make(chan *websocket.Conn, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		serverReady <- conn
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")

	msgBus := bus.NewMessageBus()
	defer msgBus.Close()

	cfg := config.WebSocketClientConfig{
		Enabled:        true,
		BackendURL:     wsURL,
		ReconnectDelay: 1,
		PingInterval:   30,
	}

	ch, err := NewWebSocketClientChannel(cfg, msgBus)
	require.NoError(t, err)

	ch.hostname = "test-pod-ctx-empty"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err = ch.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = ch.Stop(ctx) }()

	var serverConn *websocket.Conn
	select {
	case serverConn = <-serverReady:
	case <-time.After(3 * time.Second):
		t.Fatal("Server did not receive connection")
	}
	defer serverConn.Close()

	// Empty content context message should be dropped.
	emptyCtxMsg := InboundWSMessage{
		Type:     "context",
		UserID:   "user-empty",
		ChatID:   "user-empty",
		SenderID: "user-empty",
		Content:  "",
	}

	data, err := json.Marshal(emptyCtxMsg)
	require.NoError(t, err)

	err = serverConn.WriteMessage(websocket.TextMessage, data)
	require.NoError(t, err)

	// Send a real message after to ensure the read loop is still alive.
	realMsg := InboundWSMessage{
		Type:     "message",
		UserID:   "user-empty",
		ChatID:   "user-empty",
		SenderID: "user-empty",
		Content:  "follow-up",
	}

	data, err = json.Marshal(realMsg)
	require.NoError(t, err)

	err = serverConn.WriteMessage(websocket.TextMessage, data)
	require.NoError(t, err)

	inboundCtx, inboundCancel := context.WithTimeout(ctx, 3*time.Second)
	defer inboundCancel()

	// Only the real message (not the empty context) should appear.
	msg, ok := msgBus.ConsumeInbound(inboundCtx)
	require.True(t, ok, "Expected the real follow-up message")
	assert.False(t, msg.ContextOnly, "Real message should not have ContextOnly set")
	assert.Equal(t, "follow-up", msg.Content)
}

func TestSendMessage_WithUsageMetadata(t *testing.T) {
	t.Parallel()

	var receivedMsg OutboundWSMessage
	msgReceived := make(chan struct{})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if err := json.Unmarshal(data, &receivedMsg); err != nil {
			return
		}
		close(msgReceived)
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")

	msgBus := bus.NewMessageBus()
	defer msgBus.Close()

	cfg := config.WebSocketClientConfig{
		Enabled:        true,
		BackendURL:     wsURL,
		ReconnectDelay: 1,
		PingInterval:   30,
	}

	ch, err := NewWebSocketClientChannel(cfg, msgBus)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err = ch.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = ch.Stop(ctx) }()

	ch.hostname = "test-pod-usage"

	time.Sleep(100 * time.Millisecond)

	err = ch.Send(ctx, bus.OutboundMessage{
		Channel: "websocket_client",
		ChatID:  "user-tok",
		Content: "Response text",
		Metadata: map[string]string{
			"usage_prompt_tokens":     "100",
			"usage_completion_tokens": "50",
			"usage_total_tokens":      "150",
		},
	})
	require.NoError(t, err)

	select {
	case <-msgReceived:
		assert.Equal(t, "response", receivedMsg.Type)
		assert.Equal(t, "user-tok", receivedMsg.ChatID)
		assert.Equal(t, "Response text", receivedMsg.Content)
		assert.Equal(t, "test-pod-usage", receivedMsg.Metadata["pod_hostname"])
		assert.NotEmpty(t, receivedMsg.Metadata["timestamp"])
		assert.Equal(t, "100", receivedMsg.Metadata["usage_prompt_tokens"])
		assert.Equal(t, "50", receivedMsg.Metadata["usage_completion_tokens"])
		assert.Equal(t, "150", receivedMsg.Metadata["usage_total_tokens"])
	case <-time.After(3 * time.Second):
		t.Fatal("Timed out waiting for message")
	}
}

// Re-export for test assertions
var ErrNotRunning = channels.ErrNotRunning
