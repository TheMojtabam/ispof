// Package ws is a minimal pub/sub hub for live dashboard updates.
package ws

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Hub fans out a single broadcast channel to many websocket clients.
type Hub struct {
	mu      sync.RWMutex
	clients map[*Client]struct{}
	bcast   chan []byte
}

func NewHub() *Hub {
	return &Hub{
		clients: map[*Client]struct{}{},
		bcast:   make(chan []byte, 64),
	}
}

func (h *Hub) Run() {
	tk := time.NewTicker(2 * time.Second)
	defer tk.Stop()
	for {
		select {
		case msg := <-h.bcast:
			h.mu.RLock()
			for c := range h.clients {
				select {
				case c.send <- msg:
				default:
					// slow consumer — drop
				}
			}
			h.mu.RUnlock()
		case <-tk.C:
			// keep-alive ping; the client handler will write deadlines
		}
	}
}

// Broadcast sends an event to all clients.
func (h *Hub) Broadcast(eventType string, data any) {
	payload, err := json.Marshal(map[string]any{"type": eventType, "data": data, "ts": time.Now().Unix()})
	if err != nil {
		return
	}
	select {
	case h.bcast <- payload:
	default:
	}
}

// Register adds a client and starts its writer goroutine.
func (h *Hub) Register(conn *websocket.Conn) *Client {
	c := &Client{conn: conn, send: make(chan []byte, 16)}
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
	go c.writeLoop()
	return c
}

// Unregister removes a client.
func (h *Hub) Unregister(c *Client) {
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
	close(c.send)
}

// Client wraps a single websocket connection.
type Client struct {
	conn *websocket.Conn
	send chan []byte
}

func (c *Client) writeLoop() {
	for msg := range c.send {
		_ = c.conn.SetWriteDeadline(time.Now().Add(8 * time.Second))
		if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
			_ = c.conn.Close()
			return
		}
	}
}
