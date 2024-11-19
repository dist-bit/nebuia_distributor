package terminal

import (
	"log"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

type WebTerminal struct {
	clients    map[*websocket.Conn]bool
	broadcast  chan string
	lock       sync.Mutex
	lineBuffer []string
}

func NewWebTerminal() *WebTerminal {
	return &WebTerminal{
		clients:    make(map[*websocket.Conn]bool),
		broadcast:  make(chan string),
		lineBuffer: make([]string, 0),
	}
}

func (t *WebTerminal) Start() {
	go t.handleBroadcast()
}

func (t *WebTerminal) handleBroadcast() {
	for message := range t.broadcast {
		t.lock.Lock()
		t.lineBuffer = append(t.lineBuffer, message)
		// Mantener solo las últimas 100 líneas
		if len(t.lineBuffer) > 100 {
			t.lineBuffer = t.lineBuffer[len(t.lineBuffer)-100:]
		}

		for client := range t.clients {
			err := client.WriteJSON(struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			}{
				Type:    "log",
				Message: message,
			})
			if err != nil {
				log.Printf("Error broadcasting: %v", err)
				client.Close()
				delete(t.clients, client)
			}
		}
		t.lock.Unlock()
	}
}

func (t *WebTerminal) WriteLine(line string) {
	t.broadcast <- line
}

func (t *WebTerminal) HandleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Error upgrading to websocket: %v", err)
		return
	}

	t.lock.Lock()
	t.clients[conn] = true

	// Enviar buffer histórico al nuevo cliente
	for _, line := range t.lineBuffer {
		conn.WriteJSON(struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		}{
			Type:    "log",
			Message: line,
		})
	}
	t.lock.Unlock()
}

func (t *WebTerminal) Stop() {
	t.lock.Lock()
	defer t.lock.Unlock()

	for client := range t.clients {
		client.WriteMessage(websocket.CloseMessage, []byte{})
		client.Close()
		delete(t.clients, client)
	}
	close(t.broadcast)
}
