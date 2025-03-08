package websocket

import (
	"fmt"
	"github.com/abdorrahmani/gophel/internal/app"
	"github.com/abdorrahmani/gophel/internal/config"
	"github.com/gorilla/websocket"
	"log"
	"sync"
	"time"
)

var (
	conn     *websocket.Conn
	connLock sync.Mutex
)

type Message struct {
	Type    string      `json:"type"`
	ID      string      `json:"id"`
	Command string      `json:"command"`
	Data    interface{} `json:"data"`
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
	go listenForCommands()
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

func sendResponse(id, command string, data interface{}, err error) {
	connLock.Lock()
	defer connLock.Unlock()

	if conn == nil {
		return
	}

	resp := Message{
		Type:    "command_response",
		ID:      id,
		Command: command,
		Data:    data,
	}
	if err != nil {
		resp.Data = map[string]string{"error": err.Error()}
	}

	if err := conn.WriteJSON(resp); err != nil {
		log.Println("WebSocket response send failed:", err)
		conn = nil
	}
}

func listenForCommands() {
	for {
		connLock.Lock()
		if conn == nil {
			connLock.Unlock()
			time.Sleep(1 * time.Second) // Wait before retrying
			continue
		}

		var msg Message
		err := conn.ReadJSON(&msg)
		connLock.Unlock()

		if err != nil {
			log.Println("WebSocket read failed:", err)
			connLock.Lock()
			conn = nil
			connLock.Unlock()
			continue
		}

		if msg.Type != "command" {
			continue
		}

		switch msg.Command {
		case "build":
			id := app.Manager.GenerateAppID()
			app.Manager.StartApplication(id)
			sendResponse(id, "build", map[string]string{"id": id}, nil)

		case "rebuild":
			if err := app.Manager.RestartApplication(msg.ID); err != nil {
				sendResponse(msg.ID, "rebuild", nil, err)
			} else {
				sendResponse(msg.ID, "rebuild", "success", nil)
			}

		case "restart":
			if err := app.Manager.RestartApplication(msg.ID); err != nil {
				sendResponse(msg.ID, "restart", nil, err)
			} else {
				sendResponse(msg.ID, "restart", "success", nil)
			}

		case "start":
			app.Manager.StartApplication(msg.ID)
			sendResponse(msg.ID, "start", "success", nil)

		case "stop":
			app.Manager.StopApplication(msg.ID)
			sendResponse(msg.ID, "stop", "success", nil)

		case "status":
			if status, err := app.Manager.StatusApplication(msg.ID); err != nil {
				sendResponse(msg.ID, "status", nil, err)
			} else {
				sendResponse(msg.ID, "status", status, nil)
			}

		case "list":
			apps := app.Manager.ListApplications()
			sendResponse("system", "list", apps, nil)

		default:
			sendResponse(msg.ID, msg.Command, nil, fmt.Errorf("unknown command: %s", msg.Command))
		}
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
