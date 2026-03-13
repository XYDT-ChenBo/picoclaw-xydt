package botservice

import (
	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/channels"
	"github.com/sipeed/picoclaw/pkg/config"
)

func init() {
	channels.RegisterFactory("bot_service", func(cfg *config.Config, b *bus.MessageBus) (channels.Channel, error) {
		return NewBotServiceChannel(cfg.Channels.BotService, b)
	})
}
