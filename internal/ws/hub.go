// Package ws is the in-process push hub for R20-R25 (SPEC-04 §4.4).
// V1: single-process, map[userID]map[deviceID]*Conn. Multi-instance is a
// documented V1 constraint — see SPEC-04 R23.
package ws

import (
	"context"
	"sync"
)

// Frame is one push event sent to a connected device.
type Frame struct {
	Type          string `json:"type"`
	ConversationID string `json:"conversation_id,omitempty"`
	MessageID      string `json:"message_id,omitempty"`
	ConversationSeq uint64 `json:"conversation_seq,omitempty"`
	SenderID       string `json:"sender_id,omitempty"`
	Preview        string `json:"preview,omitempty"`
}

// Conn is one websocket connection. It is an interface so the hub can stay
// free of goroutine/pump details (the gateway HTTP handler owns the pump).
type Conn interface {
	DeviceID() string
	Send() chan<- Frame
	Close()
}

// Hub tracks live connections per user/device.
type Hub struct {
	mu    sync.RWMutex
	conns map[int64]map[string]Conn
}

// NewHub builds an empty Hub.
func NewHub() *Hub {
	return &Hub{conns: make(map[int64]map[string]Conn)}
}

// Register adds conn for (userID, deviceID). A previous connection for the
// same device is closed first (R20: later connection kicks earlier one, 4002).
func (h *Hub) Register(userID int64, conn Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	m, ok := h.conns[userID]
	if !ok {
		m = make(map[string]Conn)
		h.conns[userID] = m
	}
	if old, ok := m[conn.DeviceID()]; ok {
		old.Close()
	}
	m[conn.DeviceID()] = conn
}

// Unregister removes conn if it is still the live one for its (user, device).
func (h *Hub) Unregister(userID int64, conn Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	m, ok := h.conns[userID]
	if !ok {
		return
	}
	if cur, ok := m[conn.DeviceID()]; ok && cur == conn {
		delete(m, conn.DeviceID())
	}
	if len(m) == 0 {
		delete(h.conns, userID)
	}
}

// Deliver pushes f to every live connection for userID (R23). Unknown users
// are a silent no-op: offline delivery is filled by HTTP sync (R24).
func (h *Hub) Deliver(_ context.Context, userID int64, f Frame) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, c := range h.conns[userID] {
		select {
		case c.Send() <- f:
		default:
			// Slow-consumer: drop the frame and let the pump close the conn.
			c.Close()
		}
	}
}
