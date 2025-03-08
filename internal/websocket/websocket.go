package websocket

import (
	"github.com/abdorrahmani/gophel/internal/config"
	"github.com/gorilla/websocket"
	"log"
	"sync"
)

var (
	conn     *websocket.Conn
	connLock sync.Mutex
)

type Message struct {
	Type string      `json:"type"`
	ID   string      `json:"id"`
	Data interface{} `json:"data"`
}

func Connect(url string) error {
	connLock.Lock()
	defer connLock.Unlock()

	if conn != nil {
		return nil // Already connected
	}

	var err error
	conn, _, err = websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		conn = nil
		return err
	}
	log.Println("WebSocket connected to", url)
	return nil
}

func SendStats(id string, stats interface{}) {
	connLock.Lock()
	defer connLock.Unlock()

	if conn == nil {
		if err := Connect(config.GetWebSocketURL()); err != nil {
			log.Println("WebSocket connection failed:", err)
			return
		}
	}

	msg := Message{
		Type: "stats",
		ID:   id, // "system" for system stats, app ID for app stats
		Data: stats,
	}

	if err := conn.WriteJSON(msg); err != nil {
		log.Println("WebSocket send failed:", err)
		conn = nil // Reset connection to retry next time
	}
}

func Close() {
	connLock.Lock()
	defer connLock.Unlock()

	if conn != nil {
		conn.Close()
		conn = nil
	}
}
