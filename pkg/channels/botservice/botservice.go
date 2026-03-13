package botservice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/channels"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/logger"
)

// inboundMessage matches the iCenter-style BotService inbound payload.
// Example:
//
//	{"chatUuid":"...","topicId":123,"msgId":456,"text":"hello"}
type inboundMessage struct {
	ChatUUID string `json:"chatUuid"`
	TopicID  *int64 `json:"topicId,omitempty"`
	MsgID    *int64 `json:"msgId,omitempty"`
	Text     string `json:"text"`
}

// outboundMessage matches the iCenter-style BotService outbound payload.
// Example:
//
//	{"bo":{"chatUuid":"...","result":"...","messageId":"456","msgType":"chat"},
//	 "code":{"code":"0000","msg":"Success","msgId":"RetCode.Success"}}
type outboundMessage struct {
	BO struct {
		ChatUUID  string `json:"chatUuid"`
		Result    string `json:"result"`
		MessageID string `json:"messageId,omitempty"`
		MsgType   string `json:"msgType,omitempty"`
	} `json:"bo"`
	Code struct {
		Code  string `json:"code"`
		Msg   string `json:"msg"`
		MsgID string `json:"msgId"`
	} `json:"code"`
}

type BotServiceChannel struct {
	*channels.BaseChannel
	cfg config.BotServiceConfig

	connMu sync.Mutex
	conn   *websocket.Conn
	ctx    context.Context
	cancel context.CancelFunc

	writeMu       sync.Mutex
	lastInboundID sync.Map // chatUuid -> msgId(string)

	closed atomic.Bool

	// reconnect / heartbeat
	reconnectInitialDelay time.Duration
	reconnectMaxDelay     time.Duration
	reconnectMultiplier   float64
	heartbeatInterval     time.Duration
}

func NewBotServiceChannel(cfg config.BotServiceConfig, messageBus *bus.MessageBus) (*BotServiceChannel, error) {
	if strings.TrimSpace(cfg.WSURL) == "" {
		return nil, fmt.Errorf("bot_service ws_url is required")
	}

	base := channels.NewBaseChannel(
		"bot_service",
		cfg,
		messageBus,
		cfg.AllowFrom,
		channels.WithGroupTrigger(cfg.GroupTrigger),
		channels.WithReasoningChannelID(cfg.ReasoningChannelID),
	)

	ch := &BotServiceChannel{
		BaseChannel:           base,
		cfg:                   cfg,
		reconnectInitialDelay: 1 * time.Second,
		reconnectMaxDelay:     60 * time.Second,
		reconnectMultiplier:   2.0,
		heartbeatInterval:     30 * time.Second,
	}
	// Enable BaseChannel auto-trigger features (typing/reaction/placeholder) if implemented later.
	ch.SetOwner(ch)
	return ch, nil
}

func (c *BotServiceChannel) Start(ctx context.Context) error {
	if c.IsRunning() {
		return nil
	}

	c.ctx, c.cancel = context.WithCancel(ctx)

	if err := c.connect(); err != nil {
		return err
	}

	c.SetRunning(true)
	c.closed.Store(false)

	go c.readLoop()

	logger.InfoCF("bot_service", "BotServiceChannel started", map[string]any{
		"ws_url": sanitizeWSURLForLog(c.cfg.WSURL),
	})
	return nil
}

func (c *BotServiceChannel) Stop(ctx context.Context) error {
	if !c.IsRunning() {
		return nil
	}

	c.SetRunning(false)
	if c.cancel != nil {
		c.cancel()
	}

	c.connMu.Lock()
	conn := c.conn
	c.conn = nil
	c.connMu.Unlock()

	if conn != nil && c.closed.CompareAndSwap(false, true) {
		_ = conn.Close()
	}

	logger.InfoC("bot_service", "BotServiceChannel stopped")
	return nil
}

// connect dials the BotService WebSocket and stores the connection.
func (c *BotServiceChannel) connect() error {
	wsURL, err := c.buildWSURL()
	if err != nil {
		return err
	}

	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}
	headers := make(http.Header)
	if strings.TrimSpace(c.cfg.AccountID) != "" {
		headers.Set("X-Emp-No", strings.TrimSpace(c.cfg.AccountID))
	}

	conn, _, err := dialer.DialContext(c.ctx, wsURL, headers)
	if err != nil {
		logger.ErrorCF("bot_service", "WebSocket dial failed", map[string]any{
			"ws_url": sanitizeWSURLForLog(wsURL),
			"error":  err.Error(),
		})
		return err
	}

	c.connMu.Lock()
	c.conn = conn
	c.connMu.Unlock()

	logger.InfoCF("bot_service", "WebSocket connected", map[string]any{
		"ws_url": sanitizeWSURLForLog(wsURL),
	})
	return nil
}

func (c *BotServiceChannel) Send(ctx context.Context, msg bus.OutboundMessage) error {
	if !c.IsRunning() {
		return channels.ErrNotRunning
	}

	chatUUID := strings.TrimPrefix(msg.ChatID, "bot_service:")
	chatUUID = strings.TrimSpace(chatUUID)
	if chatUUID == "" {
		return fmt.Errorf("missing chatUuid in chat_id: %w", channels.ErrSendFailed)
	}

	var out outboundMessage
	out.BO.ChatUUID = chatUUID
	out.BO.Result = msg.Content
	out.BO.MsgType = "chat"
	if v, ok := c.lastInboundID.Load(chatUUID); ok {
		if s, ok2 := v.(string); ok2 && s != "" {
			out.BO.MessageID = s
		}
	}
	out.Code.Code = "0000"
	out.Code.Msg = "Success"
	out.Code.MsgID = "RetCode.Success"

	c.connMu.Lock()
	conn := c.conn
	c.connMu.Unlock()
	if conn == nil {
		return fmt.Errorf("bot_service websocket not connected: %w", channels.ErrSendFailed)
	}

	c.writeMu.Lock()
	err := conn.WriteJSON(out)
	c.writeMu.Unlock()
	if err != nil {
		logger.ErrorCF("bot_service", "Failed to send outbound message", map[string]any{
			"chat_uuid": chatUUID,
			"error":     err.Error(),
		})
		return err
	}
	logger.DebugCF("bot_service", "Outbound message sent", map[string]any{
		"chat_uuid": chatUUID,
	})
	return nil
}

func (c *BotServiceChannel) readLoop() {
	backoff := c.reconnectInitialDelay
	for {
		if c.ctx != nil {
			select {
			case <-c.ctx.Done():
				logger.InfoC("bot_service", "readLoop exiting: context done")
				return
			default:
			}
		}

		c.connMu.Lock()
		conn := c.conn
		c.connMu.Unlock()
		if conn == nil {
			if !c.IsRunning() {
				return
			}
			// try reconnect with backoff
			logger.WarnCF("bot_service", "No active connection, attempting reconnect", nil)
			if err := c.connect(); err != nil {
				time.Sleep(backoff)
				backoff = nextBackoff(backoff, c.reconnectMaxDelay, c.reconnectMultiplier)
				continue
			}
			backoff = c.reconnectInitialDelay
			continue
		}

		_, data, err := conn.ReadMessage()
		if err != nil {
			logger.WarnCF("bot_service", "WebSocket read failed", map[string]any{"error": err.Error()})
			// close current conn and trigger reconnect path
			c.connMu.Lock()
			if c.conn != nil {
				_ = c.conn.Close()
				c.conn = nil
			}
			c.connMu.Unlock()
			time.Sleep(backoff)
			backoff = nextBackoff(backoff, c.reconnectMaxDelay, c.reconnectMultiplier)
			continue
		}

		raw := strings.TrimSpace(string(data))
		if raw == "" {
			continue
		}

		// Support SSE-style frames: "data: {...}" or "data: [DONE]"
		if strings.HasPrefix(raw, "data:") {
			raw = strings.TrimSpace(strings.TrimPrefix(raw, "data:"))
			if raw == "[DONE]" {
				continue
			}
		}

		var in inboundMessage
		if err := json.Unmarshal([]byte(raw), &in); err != nil {
			logger.WarnCF("bot_service", "Inbound parse failed", map[string]any{
				"error": err.Error(),
				"raw":   truncateForLog(raw, 200),
			})
			continue
		}

		in.ChatUUID = strings.TrimSpace(in.ChatUUID)
		in.Text = strings.TrimSpace(in.Text)
		if in.ChatUUID == "" || in.Text == "" {
			continue
		}

		if in.MsgID != nil {
			c.lastInboundID.Store(in.ChatUUID, fmt.Sprintf("%d", *in.MsgID))
		}

		chatID := "bot_service:" + in.ChatUUID
		messageID := ""
		if in.MsgID != nil {
			messageID = fmt.Sprintf("%d", *in.MsgID)
		}

		peer := bus.Peer{Kind: "direct", ID: in.ChatUUID}
		sender := bus.SenderInfo{
			Platform:    "bot_service",
			PlatformID:  in.ChatUUID,
			CanonicalID: "bot_service:" + in.ChatUUID,
		}

		c.HandleMessage(
			c.ctx,
			peer,
			messageID,
			sender.CanonicalID,
			chatID,
			in.Text,
			nil,
			map[string]string{},
			sender,
		)
	}
}

// heartbeatLoop periodically sends WebSocket ping frames to keep the connection alive.
func (c *BotServiceChannel) heartbeatLoop() {
	if c.heartbeatInterval <= 0 {
		return
	}

	ticker := time.NewTicker(c.heartbeatInterval)
	defer ticker.Stop()

	for {
		if c.ctx != nil {
			select {
			case <-c.ctx.Done():
				logger.InfoC("bot_service", "heartbeatLoop exiting: context done")
				return
			case <-ticker.C:
			}
		} else {
			<-ticker.C
		}

		if !c.IsRunning() {
			return
		}

		c.connMu.Lock()
		conn := c.conn
		c.connMu.Unlock()
		if conn == nil {
			continue
		}

		deadline := time.Now().Add(5 * time.Second)
		c.writeMu.Lock()
		err := conn.WriteControl(websocket.PingMessage, []byte("ping"), deadline)
		c.writeMu.Unlock()
		if err != nil {
			logger.WarnCF("bot_service", "Heartbeat ping failed", map[string]any{
				"error": err.Error(),
			})
		}
	}
}

func (c *BotServiceChannel) buildWSURL() (string, error) {
	raw := strings.TrimSpace(c.cfg.WSURL)
	if raw == "" {
		return "", errors.New("ws_url is empty")
	}

	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if u.Scheme != "ws" && u.Scheme != "wss" {
		return "", fmt.Errorf("invalid ws_url scheme %q (expected ws/wss)", u.Scheme)
	}

	// If secret_key is provided and URL doesn't already have key=..., append it
	// to match openclaw-icenter behavior.
	secret := strings.TrimSpace(c.cfg.SecretKey)
	if secret != "" {
		q := u.Query()
		if q.Get("key") == "" {
			q.Set("key", secret)
			u.RawQuery = q.Encode()
		}
	}

	return u.String(), nil
}

func sanitizeWSURLForLog(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	if q.Get("key") != "" {
		q.Set("key", "***")
		u.RawQuery = q.Encode()
	}
	return u.String()
}

func truncateForLog(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func nextBackoff(current, max time.Duration, multiplier float64) time.Duration {
	if current <= 0 {
		current = time.Second
	}
	next := time.Duration(float64(current) * multiplier)
	if next > max {
		return max
	}
	return next
}
