// main.go
package main

import (
	"log"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
)

// ----------------------------------------------------------------------------
// Hub: maintains active clients and broadcasts messages to them.
// ----------------------------------------------------------------------------

type Hub struct {
	// Registered connections.
	clients map[*websocket.Conn]bool
	// Inbound messages from the clients.
	broadcast chan []byte
	// Register requests from the clients.
	register chan *websocket.Conn
	// Unregister requests from clients.
	unregister chan *websocket.Conn
}

func newHub() *Hub {
	return &Hub{
		clients:    make(map[*websocket.Conn]bool),
		broadcast:  make(chan []byte),
		register:   make(chan *websocket.Conn),
		unregister: make(chan *websocket.Conn),
	}
}

func (h *Hub) run() {
	for {
		select {
		case conn := <-h.register:
			h.clients[conn] = true
			log.Printf("client registered, total: %d", len(h.clients))

		case conn := <-h.unregister:
			if _, ok := h.clients[conn]; ok {
				delete(h.clients, conn)
				conn.Close()
				log.Printf("client unregistered, total: %d", len(h.clients))
			}

		case msg := <-h.broadcast:
			for conn := range h.clients {
				// send to each; if error, assume broken and unregister
				if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
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

	// Read loop: on each message, push to hub.broadcast
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			break
		}
		hub.broadcast <- msg
	}
}

func main() {
	hub := newHub()
	go hub.run()

	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		serveWs(hub, w, r)
	})

	log.Println("Broadcasting WebSocket server listening on :8080/ws")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
