package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/gorilla/websocket"
)

// ----------------------------------------------------------------------------
// Hub: maintains active clients and broadcasts messages to them with Redis persistence
// ----------------------------------------------------------------------------

type Message struct {
	User   string `json:"user"`
	Text   string `json:"text,omitempty"`
	Typing bool   `json:"typing,omitempty"`
}

type Hub struct {
	clients    map[*websocket.Conn]string
	broadcast  chan Message
	register   chan *websocket.Conn
	unregister chan *websocket.Conn
	rdb        *redis.Client
	ctx        context.Context
	maxHistory int64
}

func newHub(rdb *redis.Client) *Hub {
	return &Hub{
		clients:    make(map[*websocket.Conn]string),
		broadcast:  make(chan Message),
		register:   make(chan *websocket.Conn),
		unregister: make(chan *websocket.Conn),
		rdb:        rdb,
		ctx:        context.Background(),
		maxHistory: 100, // keep last 100 messages
	}
}

func (h *Hub) run() {
	for {
		select {
		case conn := <-h.register:
			h.clients[conn] = ""
			log.Printf("client registered, total: %d", len(h.clients))

			// Send chat history
			msgs, err := h.rdb.LRange(h.ctx, "chat_history", -h.maxHistory, -1).Result()
			if err != nil {
				log.Printf("error fetching history: %v", err)
			} else {
				for _, raw := range msgs {
					conn.WriteMessage(websocket.TextMessage, []byte(raw))
				}
			}

		case conn := <-h.unregister:
			if _, ok := h.clients[conn]; ok {
				delete(h.clients, conn)
				conn.Close()
				log.Printf("client unregistered, total: %d", len(h.clients))
			}

		case msg := <-h.broadcast:
			// Marshal and persist
			b, _ := json.Marshal(msg)
			h.rdb.RPush(h.ctx, "chat_history", b)
			h.rdb.LTrim(h.ctx, "chat_history", -h.maxHistory, -1)

			// Broadcast to active clients
			for conn := range h.clients {
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
	hub.register <- conn
	defer func() { hub.unregister <- conn }()

	// Heartbeat
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

	// First message: user joins
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

	// Read loop
	for {
		_, msgData, err := conn.ReadMessage()
		if err != nil {
			log.Printf("read error: %v", err)
			continue
		}
		var incoming Message
		if err := json.Unmarshal(msgData, &incoming); err != nil {
			continue
		}
		incoming.User = hub.clients[conn]
		hub.broadcast <- incoming
	}
}

func main() {
	// Initialize Redis client
	rdb := redis.NewClient(&redis.Options{
		Addr: "localhost:6379",
	})
	defer rdb.Close()

	hub := newHub(rdb)
	go hub.run()

	http.Handle("/", http.FileServer(http.Dir(".")))
	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		serveWs(hub, w, r)
	})

	log.Println("Broadcasting WebSocket server with Redis persistence listening on :8080/ws")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
