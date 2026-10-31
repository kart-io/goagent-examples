// Package hostws demonstrates host-owned upgrades using only httpagent's public
// connection interface. Gorilla is this example host's chosen frame engine.
package hostws

import (
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
	"github.com/kart-io/goagent-contrib/httpagent"
)

// NewHandler can be mounted at /api/ws by the example host. Authentication and
// handshake policy belong here; the core receives an already upgraded socket.
func NewHandler(h *httpagent.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "GET required", http.StatusMethodNotAllowed)
			return
		}
		selected := false
		for _, p := range websocket.Subprotocols(r) {
			if p == "goagent.v1" {
				selected = true
			}
		}
		if !selected {
			w.Header().Set("Sec-WebSocket-Protocol", "goagent.v1")
			http.Error(w, "goagent.v1 required", http.StatusUpgradeRequired)
			return
		}
		// Gorilla's default same-origin check is kept. A real framework adapter must
		// perform equivalent checks and implement its own authentication here.
		upgrader := websocket.Upgrader{Subprotocols: []string{"goagent.v1"}, HandshakeTimeout: 10 * time.Second}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		// ServeWebSocket takes ownership, including rejection/close during shutdown.
		if err = h.ServeWebSocket(r.Context(), &connection{conn: conn}); err != nil {
			log.Printf("宿主 WebSocket 连接结束: %v", err)
		}
	})
}

type connection struct{ conn *websocket.Conn }

var _ httpagent.WebSocketConn = (*connection)(nil)

func (c *connection) ReadText() ([]byte, error) {
	kind, data, err := c.conn.ReadMessage()
	if err != nil {
		return nil, err
	}
	if kind != websocket.TextMessage {
		_ = c.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseUnsupportedData, "JSON text required"), time.Now().Add(10*time.Second))
		return nil, errors.New("JSON text required")
	}
	return data, nil
}
func (c *connection) WriteText(data []byte) error {
	return c.conn.WriteMessage(websocket.TextMessage, data)
}
func (c *connection) Ping(deadline time.Time) error {
	return c.conn.WriteControl(websocket.PingMessage, nil, deadline)
}
func (c *connection) SetReadLimit(n int64)               { c.conn.SetReadLimit(n) }
func (c *connection) SetReadDeadline(t time.Time) error  { return c.conn.SetReadDeadline(t) }
func (c *connection) SetWriteDeadline(t time.Time) error { return c.conn.SetWriteDeadline(t) }
func (c *connection) SetPongHandler(f func() error) {
	c.conn.SetPongHandler(func(string) error { return f() })
}
func (c *connection) Close() error { return c.conn.Close() }
