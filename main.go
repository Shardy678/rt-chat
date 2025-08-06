package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/golang-jwt/jwt/v5"
	"github.com/gorilla/websocket"
	"golang.org/x/crypto/bcrypt"
)

// ----------------------------------------------------------------------------
// Hub: maintains active clients and broadcasts messages to them with Redis persistence
// ----------------------------------------------------------------------------

type Hub struct {
	rooms map[string]*Room
	rdb   *redis.Client
	ctx   context.Context
}

type Message struct {
	Room   string `json:"room"`
	User   string `json:"user"`
	Text   string `json:"text,omitempty"`
	Typing bool   `json:"typing,omitempty"`
}

type Room struct {
	name       string
	clients    map[*websocket.Conn]string
	broadcast  chan Message
	register   chan *websocket.Conn
	unregister chan *websocket.Conn
	rdb        *redis.Client
	ctx        context.Context
	maxHistory int64
}

func newRoom(name string, rdb *redis.Client, ctx context.Context, maxHistory int64) *Room {
	return &Room{
		name:       name,
		clients:    make(map[*websocket.Conn]string),
		broadcast:  make(chan Message),
		register:   make(chan *websocket.Conn),
		unregister: make(chan *websocket.Conn),
		rdb:        rdb,
		ctx:        ctx,
		maxHistory: maxHistory, // keep last 100 messages
	}
}
func newHub(rdb *redis.Client) *Hub {
	return &Hub{
		rooms: make(map[string]*Room),
		rdb:   rdb,
		ctx:   context.Background(),
	}
}

func (hub *Hub) getOrCreateRoom(name string) *Room {
	room, exists := hub.rooms[name]
	if !exists {
		room = newRoom(name, hub.rdb, hub.ctx, 100)
		hub.rooms[name] = room
		go room.run()
	}
	return room
}

func (r *Room) run() {
	for {
		select {
		case conn := <-r.register:
			r.clients[conn] = ""
			log.Printf("[Room:%s] client registered, total: %d", r.name, len(r.clients))

			key := "chat_history:" + r.name
			msgs, err := r.rdb.LRange(r.ctx, key, -r.maxHistory, -1).Result()
			if err != nil {
				log.Printf("[Room:%s] error fetching history: %v", r.name, err)
			} else {
				log.Printf("[Room:%s] sending %d history messages to new client", r.name, len(msgs))
				for _, raw := range msgs {
					if err := conn.WriteMessage(websocket.TextMessage, []byte(raw)); err != nil {
						log.Printf("[Room:%s] error sending history to client: %v", r.name, err)
					}
				}
			}

		case conn := <-r.unregister:
			if _, ok := r.clients[conn]; ok {
				delete(r.clients, conn)
				conn.Close()
				log.Printf("[Room:%s] client unregistered, total: %d", r.name, len(r.clients))
			}

		case msg := <-r.broadcast:
			b, err := json.Marshal(msg)
			if err != nil {
				log.Printf("[Room:%s] error marshaling message: %v", r.name, err)
				continue
			}
			key := "chat_history:" + r.name
			if err := r.rdb.RPush(r.ctx, key, b).Err(); err != nil {
				log.Printf("[Room:%s] error persisting message: %v", r.name, err)
			}
			if err := r.rdb.LTrim(r.ctx, key, -r.maxHistory, -1).Err(); err != nil {
				log.Printf("[Room:%s] error trimming history: %v", r.name, err)
			}

			for conn := range r.clients {
				if err := conn.WriteMessage(websocket.TextMessage, b); err != nil {
					log.Printf("[Room:%s] broadcast error: %v", r.name, err)
					r.unregister <- conn
				} else {
					log.Printf("[Room:%s] broadcasted message from user %q to client", r.name, msg.User)
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
	roomName := r.URL.Query().Get("room")
	if roomName == "" {
		roomName = "default"
	}
	log.Printf("[WS] Serving WebSocket for room %q", roomName)
	log.Println("[WS] New WebSocket connection attempt")
	// Validate JWT token
	cookie, err := r.Cookie("token")
	if err != nil {
		log.Printf("[WS] missing token: %v", err)
		http.Error(w, "missing token", http.StatusUnauthorized)
		return
	}
	user, err := validateJWT(cookie.Value)
	if err != nil {
		log.Printf("[WS] invalid token: %v", err)
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}
	log.Printf("[WS] JWT validated for user %q", user)

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[WS] upgrade: %v", err)
		return
	}
	log.Println("[WS] WebSocket upgraded")
	// Register user immediately and associate user with connection
	room := hub.getOrCreateRoom(roomName)
	room.register <- conn
	defer func() {
		log.Println("[WS] WebSocket closing/unregistering")
		room.unregister <- conn
	}()

	log.Printf("[WS] user %q joined", user)

	// Heartbeat
	conn.SetReadDeadline(time.Now().Add(pongWait))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(pongWait))
		log.Println("[WS] Pong received, deadline extended")
		return nil
	})

	// Ping loop
	ticker := time.NewTicker(pingPeriod)
	defer ticker.Stop()
	go func() {
		for range ticker.C {
			if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second)); err != nil {
				log.Printf("[WS] Ping error: %v", err)
				return
			}
			log.Println("[WS] Ping sent")
		}
	}()

	// Main read loop (no join message needed)
	for {
		_, msgData, err := conn.ReadMessage()
		if err != nil {
			log.Printf("[WS] read error: %v", err)
			return
		}
		var incoming Message
		if err := json.Unmarshal(msgData, &incoming); err != nil {
			log.Printf("[WS] invalid message format: %v", err)
			continue
		}
		incoming.User = user
		incoming.Room = roomName
		log.Printf("[WS] received message from user %q: %+v", incoming.User, incoming)
		room.broadcast <- incoming
	}
}

var jwtSecret = []byte("a_strong_secret")

func generateJWT(user string) (string, error) {
	log.Printf("[JWT] Generating token for user %q", user)
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"user": user,
		"exp":  time.Now().Add(24 * time.Hour).Unix(),
	})
	return token.SignedString(jwtSecret)
}

func validateJWT(tokenString string) (string, error) {
	log.Printf("[JWT] Validating token")
	token, err := jwt.Parse(tokenString, func(token *jwt.Token) (any, error) {
		return jwtSecret, nil
	})
	if err != nil || !token.Valid {
		log.Printf("[JWT] Token invalid: %v", err)
		return "", err
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		log.Printf("[JWT] Token malformed")
		return "", jwt.ErrTokenMalformed
	}
	if exp, ok := claims["exp"].(float64); ok && time.Now().Unix() > int64(exp) {
		log.Printf("[JWT] Token expired")
		return "", jwt.ErrTokenExpired
	}
	user, ok := claims["user"].(string)
	if !ok || user == "" {
		log.Printf("[JWT] Token missing user")
		return "", jwt.ErrTokenMalformed
	}
	log.Printf("[JWT] Token valid for user %q", user)
	return user, nil
}

func hashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	return string(hash), err
}

func checkPasswordHash(password, hash string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

func registerHandler(rdb *redis.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			User     string `json:"user"`
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.User == "" || req.Password == "" {
			http.Error(w, "invalid user or password", http.StatusBadRequest)
			return
		}
		// Check if user exists
		exists, err := rdb.HExists(context.Background(), "users", req.User).Result()
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if exists {
			http.Error(w, "user already exists", http.StatusBadRequest)
			return
		}
		hash, err := hashPassword(req.Password)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if err := rdb.HSet(context.Background(), "users", req.User, hash).Err(); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"registered"}`))
	}
}

func loginHandler(rdb *redis.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Println("[Login] Received login request")
		var req struct {
			User     string `json:"user"`
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.User == "" || req.Password == "" {
			log.Printf("[Login] Invalid request payload: %v", err)
			http.Error(w, "invalid user or password", http.StatusBadRequest)
			return
		}
		hash, err := rdb.HGet(context.Background(), "users", req.User).Result()
		if err == redis.Nil {
			http.Error(w, "invalid credentials", http.StatusUnauthorized)
			return
		} else if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if !checkPasswordHash(req.Password, hash) {
			http.Error(w, "invalid credentials", http.StatusUnauthorized)
			return
		}
		token, err := generateJWT(req.User)
		if err != nil {
			log.Printf("[Login] Could not create token for user %q: %v", req.User, err)
			http.Error(w, "could not create token", http.StatusInternalServerError)
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name:     "token",
			Value:    token,
			Path:     "/",
			HttpOnly: true,
			Secure:   false,
			SameSite: http.SameSiteStrictMode,
		})
		w.Header().Set("Content-Type", "application/json")
		log.Printf("[Login] User %q logged in successfully", req.User)
		json.NewEncoder(w).Encode(map[string]string{"token": token})
	}
}

func main() {
	log.Println("[Main] Initializing Redis client")
	// Initialize Redis client
	rdb := redis.NewClient(&redis.Options{
		Addr: "localhost:6379",
	})
	defer func() {
		log.Println("[Main] Closing Redis client")
		rdb.Close()
	}()

	hub := newHub(rdb)
	log.Println("[Main] Hub started")

	fs := http.FileServer(http.Dir("./static"))
	http.Handle("/static/", http.StripPrefix("/static/", fs))
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "index.html")
	})
	http.Handle("/register", registerHandler(rdb))
	http.Handle("/login", loginHandler(rdb))
	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		serveWs(hub, w, r)
	})

	log.Println("[Main] Broadcasting WebSocket server with Redis persistence listening on :8080/ws")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
