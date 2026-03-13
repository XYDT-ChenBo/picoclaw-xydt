package botservice

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
)

func TestBotServiceChannel_InboundAndOutbound(t *testing.T) {
	t.Parallel()

	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}

	serverConnCh := make(chan *websocket.Conn, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		serverConnCh <- c
	}))
	t.Cleanup(srv.Close)

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	mb := bus.NewMessageBus()
	cfg := config.BotServiceConfig{
		Enabled: true,
		WSURL:   wsURL,
	}

	ch, err := NewBotServiceChannel(cfg, mb)
	if err != nil {
		t.Fatalf("NewBotServiceChannel: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := ch.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = ch.Stop(context.Background()) })

	var serverConn *websocket.Conn
	select {
	case serverConn = <-serverConnCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for ws connect")
	}
	t.Cleanup(func() { _ = serverConn.Close() })

	// 1) Server sends inbound message
	in := inboundMessage{
		ChatUUID: "chat-1",
		MsgID:    ptrI64(456),
		Text:     "hello",
	}
	if err := serverConn.WriteJSON(in); err != nil {
		t.Fatalf("server WriteJSON inbound: %v", err)
	}

	// 2) Verify it arrived on MessageBus
	inboundCtx, inboundCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer inboundCancel()
	got, ok := mb.ConsumeInbound(inboundCtx)
	if !ok {
		t.Fatal("expected inbound message, got none")
	}
	if got.Channel != "bot_service" {
		t.Fatalf("got.Channel = %q", got.Channel)
	}
	if got.ChatID != "bot_service:chat-1" {
		t.Fatalf("got.ChatID = %q", got.ChatID)
	}
	if got.Content != "hello" {
		t.Fatalf("got.Content = %q", got.Content)
	}

	// 3) Call Send, expect outbound JSON with messageId=456
	if err := ch.Send(context.Background(), bus.OutboundMessage{
		Channel: "bot_service",
		ChatID:  "bot_service:chat-1",
		Content: "world",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	_, raw, err := serverConn.ReadMessage()
	if err != nil {
		t.Fatalf("server ReadMessage outbound: %v", err)
	}
	var out outboundMessage
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal outbound: %v, raw=%s", err, string(raw))
	}
	if out.BO.ChatUUID != "chat-1" {
		t.Fatalf("out.bo.chatUuid=%q", out.BO.ChatUUID)
	}
	if out.BO.Result != "world" {
		t.Fatalf("out.bo.result=%q", out.BO.Result)
	}
	if out.BO.MessageID != "456" {
		t.Fatalf("out.bo.messageId=%q", out.BO.MessageID)
	}
	if out.Code.Code != "0000" {
		t.Fatalf("out.code.code=%q", out.Code.Code)
	}
}

func ptrI64(v int64) *int64 { return &v }
