package websocket_client

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/channels"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/logger"
)

const (
	channelName = "websocket_client"

	defaultReconnectDelay = 5  // seconds
	defaultPingInterval   = 30 // seconds
	maxReconnectDelay     = 60 // seconds
	writeTimeout          = 10 * time.Second
	handshakeTimeout      = 10 * time.Second
	jitterMax             = time.Second // random jitter [0, 1s) to prevent thundering herd

	// Small WS buffers for low-spec K8s pods
	readBufferSize  = 1024
	writeBufferSize = 1024
)

// InboundWSMessage is the JSON message from backend to PicoClaw.
type InboundWSMessage struct {
	Type     string            `json:"type"`
	UserID   string            `json:"user_id"`
	ChatID   string            `json:"chat_id"`
	SenderID string            `json:"sender_id"`
	Content  string            `json:"content"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

// OutboundWSMessage is the JSON message from PicoClaw to backend.
type OutboundWSMessage struct {
	Type     string            `json:"type"`
	UserID   string            `json:"user_id"`
	ChatID   string            `json:"chat_id"`
	Content  string            `json:"content"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

// WebSocketClientChannel connects to a backend WS server for bidirectional communication.
type WebSocketClientChannel struct {
	*channels.BaseChannel
	cfg      config.WebSocketClientConfig
	conn     *websocket.Conn
	connMu   sync.Mutex // guards conn read/replace
	writeMu  sync.Mutex // guards conn writes
	ctx      context.Context
	cancel   context.CancelFunc
	hostname string
}

// NewWebSocketClientChannel creates a new WebSocket client channel.
func NewWebSocketClientChannel(
	cfg config.WebSocketClientConfig,
	messageBus *bus.MessageBus,
) (*WebSocketClientChannel, error) {
	if cfg.BackendURL == "" {
		return nil, fmt.Errorf("websocket_client: backend_url is required")
	}

	base := channels.NewBaseChannel(channelName, cfg, messageBus, cfg.AllowFrom)

	return &WebSocketClientChannel{
		BaseChannel: base,
		cfg:         cfg,
	}, nil
}

func (c *WebSocketClientChannel) Start(ctx context.Context) error {
	if c.IsRunning() {
		return fmt.Errorf("websocket_client: channel already running")
	}

	c.hostname = os.Getenv("HOSTNAME")
	if c.hostname == "" {
		c.hostname = "unknown"
	}

	logger.InfoCF(channelName, "Starting WebSocket client channel", map[string]any{
		"backend_url":  c.cfg.BackendURL,
		"pod_hostname": c.hostname,
	})

	c.ctx, c.cancel = context.WithCancel(ctx)

	if err := c.connect(); err != nil {
		logger.WarnCF(channelName, "Initial connection failed, will retry in background", map[string]any{
			"error": err.Error(),
		})
	} else {
		go c.readLoop()
		go c.pingLoop()
	}

	go c.reconnectLoop()

	c.SetRunning(true)
	logger.InfoC(channelName, "WebSocket client channel started")
	return nil
}

func (c *WebSocketClientChannel) Stop(ctx context.Context) error {
	logger.InfoC(channelName, "Stopping WebSocket client channel")
	c.SetRunning(false)

	if c.cancel != nil {
		c.cancel()
	}

	c.connMu.Lock()
	if c.conn != nil {
		// Send close frame for graceful shutdown
		deadline := time.Now().Add(time.Second)
		closeMsg := websocket.FormatCloseMessage(websocket.CloseNormalClosure, "shutdown")
		_ = c.conn.WriteControl(websocket.CloseMessage, closeMsg, deadline)
		c.conn.Close()
		c.conn = nil
	}
	c.connMu.Unlock()

	return nil
}

func (c *WebSocketClientChannel) Send(ctx context.Context, msg bus.OutboundMessage) error {
	if !c.IsRunning() {
		return channels.ErrNotRunning
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	outMsg := OutboundWSMessage{
		Type:    "response",
		UserID:  msg.ChatID,
		ChatID:  msg.ChatID,
		Content: msg.Content,
		Metadata: map[string]string{
			"pod_hostname": c.hostname,
			"timestamp":    time.Now().UTC().Format(time.RFC3339),
		},
	}

	data, err := json.Marshal(outMsg)
	if err != nil {
		return fmt.Errorf("websocket_client: marshal failed: %w", channels.ErrSendFailed)
	}

	c.connMu.Lock()
	conn := c.conn
	if conn == nil {
		c.connMu.Unlock()
		return fmt.Errorf("websocket_client: not connected: %w", channels.ErrTemporary)
	}
	c.writeMu.Lock()
	c.connMu.Unlock()

	_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	err = conn.WriteMessage(websocket.TextMessage, data)
	_ = conn.SetWriteDeadline(time.Time{})
	c.writeMu.Unlock()

	if err != nil {
		logger.ErrorCF(channelName, "Failed to send message", map[string]any{
			"chat_id": msg.ChatID,
			"error":   err.Error(),
		})
		return fmt.Errorf("websocket_client: write failed: %w", channels.ErrTemporary)
	}

	logger.DebugCF(channelName, "Message sent", map[string]any{
		"chat_id": msg.ChatID,
		"length":  len(msg.Content),
	})

	return nil
}

func (c *WebSocketClientChannel) connect() error {
	u, err := url.Parse(c.cfg.BackendURL)
	if err != nil {
		return fmt.Errorf("invalid backend_url: %w", err)
	}

	q := u.Query()
	q.Set("pod_hostname", c.hostname)
	u.RawQuery = q.Encode()

	dialer := websocket.Dialer{
		HandshakeTimeout: handshakeTimeout,
		ReadBufferSize:   readBufferSize,
		WriteBufferSize:  writeBufferSize,
	}

	header := http.Header{}
	if c.cfg.AuthToken != "" {
		header.Set("Authorization", "Bearer "+c.cfg.AuthToken)
	}

	conn, resp, err := dialer.DialContext(c.ctx, u.String(), header)
	if resp != nil {
		resp.Body.Close()
	}
	if err != nil {
		return fmt.Errorf("websocket dial failed: %w", err)
	}

	pingInterval := c.pingIntervalDuration()
	conn.SetPongHandler(func(_ string) error {
		return conn.SetReadDeadline(time.Now().Add(2 * pingInterval))
	})
	_ = conn.SetReadDeadline(time.Now().Add(2 * pingInterval))

	c.connMu.Lock()
	c.conn = conn
	c.connMu.Unlock()

	logger.InfoCF(channelName, "Connected to backend", map[string]any{
		"url":          u.String(),
		"pod_hostname": c.hostname,
	})

	return nil
}

func (c *WebSocketClientChannel) readLoop() {
	c.connMu.Lock()
	conn := c.conn
	c.connMu.Unlock()

	if conn == nil {
		return
	}

	for {
		select {
		case <-c.ctx.Done():
			return
		default:
		}

		_, message, err := conn.ReadMessage()
		if err != nil {
			// Check if we're shutting down
			select {
			case <-c.ctx.Done():
				return
			default:
			}

			logger.WarnCF(channelName, "Read error, connection lost", map[string]any{
				"error": err.Error(),
			})

			c.connMu.Lock()
			if c.conn == conn {
				c.conn.Close()
				c.conn = nil
			}
			c.connMu.Unlock()
			return
		}

		var inMsg InboundWSMessage
		if err := json.Unmarshal(message, &inMsg); err != nil {
			logger.WarnCF(channelName, "Failed to unmarshal inbound message", map[string]any{
				"error":  err.Error(),
				"length": len(message),
			})
			continue
		}

		if inMsg.Content == "" {
			logger.DebugCF(channelName, "Received empty content, ignoring", nil)
			continue
		}

		chatID := inMsg.ChatID
		if chatID == "" {
			chatID = inMsg.UserID
		}

		senderID := inMsg.SenderID
		if senderID == "" {
			senderID = inMsg.UserID
		}

		logger.InfoCF(channelName, "Received message", map[string]any{
			"user_id":   inMsg.UserID,
			"chat_id":   chatID,
			"sender_id": senderID,
			"type":      inMsg.Type,
			"length":    len(inMsg.Content),
		})

		peer := bus.Peer{Kind: "direct", ID: senderID}
		c.HandleMessage(c.ctx, peer, "", senderID, chatID, inMsg.Content, nil, inMsg.Metadata)
	}
}

func (c *WebSocketClientChannel) pingLoop() {
	interval := c.pingIntervalDuration()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			c.connMu.Lock()
			conn := c.conn
			c.connMu.Unlock()

			if conn == nil {
				return
			}

			c.writeMu.Lock()
			err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(writeTimeout))
			c.writeMu.Unlock()

			if err != nil {
				logger.DebugCF(channelName, "Ping failed", map[string]any{
					"error": err.Error(),
				})
				return
			}
		}
	}
}

func (c *WebSocketClientChannel) reconnectLoop() {
	baseDelay := c.reconnectDelayDuration()
	attempt := 0

	for {
		select {
		case <-c.ctx.Done():
			return
		case <-time.After(baseDelay):
		}

		c.connMu.Lock()
		conn := c.conn
		c.connMu.Unlock()

		if conn != nil {
			attempt = 0
			continue
		}

		delay := c.backoffDelay(baseDelay, attempt)
		logger.InfoCF(channelName, "Attempting to reconnect", map[string]any{
			"attempt": attempt + 1,
			"delay":   delay.String(),
		})

		select {
		case <-c.ctx.Done():
			return
		case <-time.After(delay):
		}

		if err := c.connect(); err != nil {
			logger.WarnCF(channelName, "Reconnect failed", map[string]any{
				"attempt": attempt + 1,
				"error":   err.Error(),
			})
			attempt++
			continue
		}

		attempt = 0
		go c.readLoop()
		go c.pingLoop()

		logger.InfoC(channelName, "Successfully reconnected to backend")
	}
}

func (c *WebSocketClientChannel) backoffDelay(baseDelay time.Duration, attempt int) time.Duration {
	delay := baseDelay
	for i := 0; i < attempt; i++ {
		delay *= 2
		if delay > time.Duration(maxReconnectDelay)*time.Second {
			delay = time.Duration(maxReconnectDelay) * time.Second
			break
		}
	}
	// Add jitter to prevent thundering herd
	jitter := time.Duration(rand.Int64N(int64(jitterMax)))
	return delay + jitter
}

func (c *WebSocketClientChannel) pingIntervalDuration() time.Duration {
	interval := c.cfg.PingInterval
	if interval <= 0 {
		interval = defaultPingInterval
	}
	return time.Duration(interval) * time.Second
}

func (c *WebSocketClientChannel) reconnectDelayDuration() time.Duration {
	delay := c.cfg.ReconnectDelay
	if delay <= 0 {
		delay = defaultReconnectDelay
	}
	return time.Duration(delay) * time.Second
}
