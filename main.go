// main.go
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
)

// ----------------------------------------------------------------------------
// Hub: maintains active clients and broadcasts messages to them.
// ----------------------------------------------------------------------------

type Message struct {
	User string `json:"user"`
	Text string `json:"text"`
}

type Hub struct {
	// Registered connections.
	clients map[*websocket.Conn]string
	// Inbound messages from the clients.
	broadcast chan Message
	// Register requests from the clients.
	register chan *websocket.Conn
	// Unregister requests from clients.
	unregister chan *websocket.Conn
}

func newHub() *Hub {
	return &Hub{
		clients:    make(map[*websocket.Conn]string),
		broadcast:  make(chan Message),
		register:   make(chan *websocket.Conn),
		unregister: make(chan *websocket.Conn),
	}
}

func (h *Hub) run() {
	for {
		select {
		case conn := <-h.register:
			h.clients[conn] = ""
			log.Printf("client registered, total: %d", len(h.clients))

		case conn := <-h.unregister:
			if _, ok := h.clients[conn]; ok {
				delete(h.clients, conn)
				conn.Close()
				log.Printf("client unregistered, total: %d", len(h.clients))
			}

		case msg := <-h.broadcast:
			b, _ := json.Marshal(msg)
			for conn := range h.clients {
				// send to each; if error, assume broken and unregister
				if err := conn.WriteMessage(websocket.TextMessage, b); err != nil {
					log.Printf("broadcast error: %v", err)
					h.unregister <- conn
				}
			}
		}
	}
}

// ----------------------------------------------------------------------------
// WebSocket server with ping/pong + registration in hub.
// ----------------------------------------------------------------------------

const (
	pongWait   = 60 * time.Second
	pingPeriod = (pongWait * 9) / 10
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func serveWs(hub *Hub, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("upgrade: %v", err)
		return
	}
	// Register this client
	hub.register <- conn

	// Ensure cleanup
	defer func() {
		hub.unregister <- conn
	}()

	// Set up heartbeat
	conn.SetReadDeadline(time.Now().Add(pongWait))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	// Ping loop
	ticker := time.NewTicker(pingPeriod)
	defer ticker.Stop()
	go func() {
		for range ticker.C {
			if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second)); err != nil {
				return
			}
		}
	}()
	_, userMsg, err := conn.ReadMessage()
	if err != nil {
		return
	}
	var userObj Message
	if err := json.Unmarshal(userMsg, &userObj); err != nil || userObj.User == "" {
		return
	}
	hub.clients[conn] = userObj.User
	log.Printf("user %q joined", userObj.User)

	// Read loop: on each message, push to hub.broadcast
	for {
		_, msgData, err := conn.ReadMessage()
		if err != nil {
			log.Printf("read error: %v", err)
			continue
		}
		var msg Message
		if err := json.Unmarshal(msgData, &msg); err == nil {
			// Always use the username associated with the connection (not what client sends)
			msg.User = hub.clients[conn]
			hub.broadcast <- msg
		}
	}
}

func main() {
	hub := newHub()
	go hub.run()

	http.Handle("/", http.FileServer(http.Dir(".")))

	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		serveWs(hub, w, r)
	})

	log.Println("Broadcasting WebSocket server listening on :8080/ws")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
