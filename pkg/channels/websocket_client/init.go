package websocket_client

import (
	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/channels"
	"github.com/sipeed/picoclaw/pkg/config"
)

func init() {
	channels.RegisterFactory("websocket_client", func(cfg *config.Config, b *bus.MessageBus) (channels.Channel, error) {
		return NewWebSocketClientChannel(cfg.Channels.WebSocketClient, b)
	})
}
