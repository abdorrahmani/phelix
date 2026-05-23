package monitor

import (
	"fmt"
	"time"

	"github.com/abdorrahmani/phelix/config"
	"github.com/gorilla/websocket"
)

type websocketConn struct {
	conn *websocket.Conn
}

func (w *websocketConn) Connect(token, sessionID string) error {
	cfg := config.Get()
	dialer := websocket.Dialer{
		HandshakeTimeout: handshakeTimeout,
		ReadBufferSize:   32768,
		WriteBufferSize:  32768,
	}
	url := fmt.Sprintf("%s?token=%s&sessionID=%s", cfg.App.WSSUrl+"/phelix/ws", token, sessionID)
	conn, _, err := dialer.Dial(url, nil)
	if err != nil {
		return fmt.Errorf("failed to connect to WebSocket: %w", err)
	}
	w.conn = conn
	return nil
}

func (w *websocketConn) Close() error {
	if w.conn != nil {
		return w.conn.Close()
	}
	return nil
}

func (w *websocketConn) WriteJSON(v interface{}) error {
	return w.conn.WriteJSON(v)
}

func (w *websocketConn) ReadJSON(v interface{}) error {
	return w.conn.ReadJSON(v)
}

func (w *websocketConn) WriteMessage(messageType int, data []byte) error {
	return w.conn.WriteMessage(messageType, data)
}

func (w *websocketConn) SetReadDeadline(t time.Time) error {
	return w.conn.SetReadDeadline(t)
}

func (w *websocketConn) SetWriteDeadline(t time.Time) error {
	return w.conn.SetWriteDeadline(t)
}

func (w *websocketConn) SetPongHandler(h func(string) error) {
	w.conn.SetPongHandler(h)
}
