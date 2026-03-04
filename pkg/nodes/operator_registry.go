package nodes

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// OperatorSession holds the state of a connected operator client.
type OperatorSession struct {
	ConnID    string
	Scopes    []string
	ClientID  string
	Conn      *websocket.Conn
	ConnectedAtMs int64
	mu        sync.Mutex
}

// Send sends a JSON frame to this operator.
func (s *OperatorSession) Send(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Conn.WriteMessage(websocket.TextMessage, data)
}

// OperatorRegistry tracks connected operator WebSocket clients.
type OperatorRegistry struct {
	mu      sync.RWMutex
	byConn  map[string]*OperatorSession // connID -> session
}

// NewOperatorRegistry creates a new OperatorRegistry.
func NewOperatorRegistry() *OperatorRegistry {
	return &OperatorRegistry{
		byConn: make(map[string]*OperatorSession),
	}
}

// Register adds a new operator session.
func (r *OperatorRegistry) Register(connID string, conn *websocket.Conn, params *ConnectParams) *OperatorSession {
	scopes := params.Scopes
	if scopes == nil {
		scopes = []string{}
	}
	clientID := ""
	if params.Device != nil && params.Device.ID != "" {
		clientID = params.Device.ID
	} else if params.Client.InstanceID != "" {
		clientID = params.Client.InstanceID
	} else {
		clientID = params.Client.ID
	}
	session := &OperatorSession{
		ConnID:         connID,
		Scopes:         scopes,
		ClientID:       clientID,
		Conn:           conn,
		ConnectedAtMs:  time.Now().UnixMilli(),
	}
	r.mu.Lock()
	r.byConn[connID] = session
	r.mu.Unlock()
	return session
}

// Unregister removes an operator by connection ID.
func (r *OperatorRegistry) Unregister(connID string) {
	r.mu.Lock()
	delete(r.byConn, connID)
	r.mu.Unlock()
}

// Get returns the session for the given connection ID.
func (r *OperatorRegistry) Get(connID string) *OperatorSession {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.byConn[connID]
}

// Broadcast sends an event to all connected operators.
func (r *OperatorRegistry) Broadcast(event string, payload any) {
	frame := EventFrame{
		Type:    FrameTypeEvent,
		Event:   event,
		Payload: payload,
	}
	data, err := json.Marshal(frame)
	if err != nil {
		return
	}
	r.mu.RLock()
	sessions := make([]*OperatorSession, 0, len(r.byConn))
	for _, s := range r.byConn {
		sessions = append(sessions, s)
	}
	r.mu.RUnlock()
	for _, s := range sessions {
		s.mu.Lock()
		_ = s.Conn.WriteMessage(websocket.TextMessage, data)
		s.mu.Unlock()
	}
}

// OperatorBackend provides agent/chat processing for operator connections.
// Implemented by the gateway and passed to the nodes server.
type OperatorBackend interface {
	// ProcessMessage processes a user message and returns the agent response.
	ProcessMessage(ctx context.Context, message, sessionKey string) (string, error)
}
