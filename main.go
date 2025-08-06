package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/golang-jwt/jwt/v5"
	"github.com/gorilla/websocket"
	"golang.org/x/crypto/bcrypt"
)

// Hub управляет всеми активными комнатами чата и подключается к Redis для хранения и pub/sub сообщений.
type Hub struct {
	rooms map[string]*Room
	rdb   *redis.Client
	ctx   context.Context
}

// Message представляет собой сообщение чата или событие набора текста, отправляемое между пользователями и сохраняемое в Redis.
type Message struct {
	Room   string `json:"room"`
	User   string `json:"user"`
	Text   string `json:"text,omitempty"`
	Typing bool   `json:"typing,omitempty"`
}

// Room управляет состоянием одной комнаты чата, пользователями и каналами сообщений.
type Room struct {
	name       string
	clients    map[*websocket.Conn]string
	broadcast  chan Message
	register   chan *websocket.Conn
	unregister chan *websocket.Conn
	rdb        *redis.Client
	ctx        context.Context
	maxHistory int64
	pubsub     *redis.PubSub
}

// newRoom инициализирует комнату, подписывается на канал Redis и запускает слушателя pubsub.
func newRoom(name string, rdb *redis.Client, ctx context.Context, maxHistory int64) *Room {
	pubsub := rdb.Subscribe(ctx, "room:"+name)
	room := &Room{
		name:       name,
		clients:    make(map[*websocket.Conn]string),
		broadcast:  make(chan Message),
		register:   make(chan *websocket.Conn),
		unregister: make(chan *websocket.Conn),
		rdb:        rdb,
		ctx:        ctx,
		maxHistory: maxHistory,
		pubsub:     pubsub,
	}
	go room.listenPubSub()
	return room
}

// listenPubSub получает опубликованные сообщения из Redis и рассылает их всем клиентам комнаты.
func (r *Room) listenPubSub() {
	for msg := range r.pubsub.Channel() {
		var message Message
		if err := json.Unmarshal([]byte(msg.Payload), &message); err == nil {
			for conn := range r.clients {
				if err := conn.WriteMessage(websocket.TextMessage, []byte(msg.Payload)); err != nil {
					log.Printf("[Room:%s] ошибка отправки сообщения клиенту: %v", r.name, err)
				}
			}
		}
	}
}

// newHub создаёт новый хаб чата с backend'ом на Redis.
func newHub(rdb *redis.Client) *Hub {
	return &Hub{
		rooms: make(map[string]*Room),
		rdb:   rdb,
		ctx:   context.Background(),
	}
}

// getOrCreateRoom ищет комнату по имени или создаёт её, если она не существует.
func (hub *Hub) getOrCreateRoom(name string) *Room {
	room, exists := hub.rooms[name]
	if !exists {
		room = newRoom(name, hub.rdb, hub.ctx, 100)
		hub.rooms[name] = room
		go room.run()
	}
	return room
}

// run обрабатывает регистрацию/отмену регистрации клиентов и рассылку/сохранение сообщений для комнаты.
func (r *Room) run() {
	for {
		select {
		case conn := <-r.register:
			r.clients[conn] = ""
			log.Printf("[Room:%s] клиент зарегистрирован, всего: %d", r.name, len(r.clients))

			// Отправить историю чата при входе
			key := "chat_history:" + r.name
			msgs, err := r.rdb.LRange(r.ctx, key, -r.maxHistory, -1).Result()
			if err != nil {
				log.Printf("[Room:%s] ошибка получения истории: %v", r.name, err)
			} else {
				log.Printf("[Room:%s] отправка %d сообщений истории новому клиенту", r.name, len(msgs))
				for _, raw := range msgs {
					if err := conn.WriteMessage(websocket.TextMessage, []byte(raw)); err != nil {
						log.Printf("[Room:%s] ошибка отправки истории клиенту: %v", r.name, err)
					}
				}
			}

		case conn := <-r.unregister:
			if _, ok := r.clients[conn]; ok {
				delete(r.clients, conn)
				conn.Close()
				log.Printf("[Room:%s] клиент вышел, всего: %d", r.name, len(r.clients))
			}

		case msg := <-r.broadcast:
			b, err := json.Marshal(msg)
			if err != nil {
				log.Printf("[Room:%s] ошибка сериализации сообщения: %v", r.name, err)
				continue
			}
			key := "chat_history:" + r.name
			// Сохраняем новое сообщение и обрезаем историю до maxHistory
			if err := r.rdb.RPush(r.ctx, key, b).Err(); err != nil {
				log.Printf("[Room:%s] ошибка сохранения сообщения: %v", r.name, err)
			}
			if err := r.rdb.LTrim(r.ctx, key, -r.maxHistory, -1).Err(); err != nil {
				log.Printf("[Room:%s] ошибка обрезки истории: %v", r.name, err)
			}
			// Публикуем для других подписчиков комнаты (для масштабирования)
			if err := r.rdb.Publish(r.ctx, "room:"+r.name, b).Err(); err != nil {
				log.Printf("[Room:%s] ошибка публикации сообщения: %v", r.name, err)
			}
		}
	}
}

// Константы для поддержания активности WebSocket через ping/pong
const (
	pongWait   = 60 * time.Second
	pingPeriod = (pongWait * 9) / 10
)

// Для демо и теста разрешаем все origin. В проде ограничьте это!
var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// serveWs апгрейдит HTTP до WebSocket, аутентифицирует пользователя, регистрирует его в хабе и поддерживает соединение ping'ами.
func serveWs(hub *Hub, w http.ResponseWriter, r *http.Request) {
	roomName := r.URL.Query().Get("room")
	if roomName == "" {
		roomName = "default"
	}
	log.Printf("[WS] WebSocket для комнаты %q", roomName)

	// Проверяем JWT cookie
	cookie, err := r.Cookie("token")
	if err != nil {
		log.Printf("[WS] отсутствует токен: %v", err)
		http.Error(w, "missing token", http.StatusUnauthorized)
		return
	}
	user, err := validateJWT(cookie.Value)
	if err != nil {
		log.Printf("[WS] неверный токен: %v", err)
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}
	log.Printf("[WS] JWT прошёл проверку для пользователя %q", user)

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[WS] ошибка апгрейда: %v", err)
		return
	}
	log.Println("[WS] WebSocket подключён")

	room := hub.getOrCreateRoom(roomName)
	room.register <- conn
	defer func() {
		log.Println("[WS] Закрытие/выход из WebSocket")
		room.unregister <- conn
	}()

	log.Printf("[WS] пользователь %q присоединился", user)

	// Устанавливаем pong handler и дедлайны для keepalive
	conn.SetReadDeadline(time.Now().Add(pongWait))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(pongWait))
		log.Println("[WS] Получен pong, дедлайн обновлён")
		return nil
	})

	// Периодически отправляем ping для поддержания соединения
	ticker := time.NewTicker(pingPeriod)
	defer ticker.Stop()
	go func() {
		for range ticker.C {
			if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second)); err != nil {
				log.Printf("[WS] Ошибка ping: %v", err)
				return
			}
			log.Println("[WS] Ping отправлен")
		}
	}()

	// Основной read-цикл: получаем сообщения от клиента и отправляем их в комнату
	for {
		_, msgData, err := conn.ReadMessage()
		if err != nil {
			log.Printf("[WS] ошибка чтения: %v", err)
			return
		}
		var incoming Message
		if err := json.Unmarshal(msgData, &incoming); err != nil {
			log.Printf("[WS] неверный формат сообщения: %v", err)
			continue
		}
		incoming.User = user
		incoming.Room = roomName
		log.Printf("[WS] сообщение от пользователя %q: %+v", incoming.User, incoming)
		room.broadcast <- incoming
	}
}

var jwtSecret = []byte("a_strong_secret")

// generateJWT возвращает подписанный JWT токен для указанного пользователя.
func generateJWT(user string) (string, error) {
	log.Printf("[JWT] Генерируем токен для пользователя %q", user)
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"user": user,
		"exp":  time.Now().Add(24 * time.Hour).Unix(),
	})
	return token.SignedString(jwtSecret)
}

// validateJWT парсит и валидирует JWT, возвращает имя пользователя если токен валиден.
func validateJWT(tokenString string) (string, error) {
	log.Printf("[JWT] Проверяем токен")
	token, err := jwt.Parse(tokenString, func(token *jwt.Token) (any, error) {
		return jwtSecret, nil
	})
	if err != nil || !token.Valid {
		log.Printf("[JWT] Токен невалиден: %v", err)
		return "", err
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		log.Printf("[JWT] Токен повреждён")
		return "", jwt.ErrTokenMalformed
	}
	if exp, ok := claims["exp"].(float64); ok && time.Now().Unix() > int64(exp) {
		log.Printf("[JWT] Токен истёк")
		return "", jwt.ErrTokenExpired
	}
	user, ok := claims["user"].(string)
	if !ok || user == "" {
		log.Printf("[JWT] В токене нет пользователя")
		return "", jwt.ErrTokenMalformed
	}
	log.Printf("[JWT] Токен валиден для пользователя %q", user)
	return user, nil
}

// hashPassword хэширует пароль для хранения в базе.
func hashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	return string(hash), err
}

// checkPasswordHash сравнивает plaintext-пароль с его хэшем.
func checkPasswordHash(password, hash string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// registerHandler обрабатывает регистрацию новых пользователей через JSON POST.
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
		// Проверяем, есть ли уже такой пользователь
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

// loginHandler обрабатывает логин, проверяет данные и устанавливает JWT cookie.
func loginHandler(rdb *redis.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Println("[Login] Получен запрос на вход")
		var req struct {
			User     string `json:"user"`
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.User == "" || req.Password == "" {
			log.Printf("[Login] Некорректный payload: %v", err)
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
			log.Printf("[Login] Не удалось создать токен для пользователя %q: %v", req.User, err)
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
		log.Printf("[Login] Пользователь %q успешно вошёл", req.User)
		json.NewEncoder(w).Encode(map[string]string{"token": token})
	}
}

func main() {
	log.Println("[Main] Инициализация клиента Redis")
	// Берём адрес Redis из env или по умолчанию
	redisAddr := os.Getenv("REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "localhost:6379"
	}
	rdb := redis.NewClient(&redis.Options{
		Addr: redisAddr,
	})
	defer func() {
		log.Println("[Main] Закрытие клиента Redis")
		rdb.Close()
	}()

	hub := newHub(rdb)
	log.Println("[Main] Хаб запущен")

	// Сервер статических файлов для фронтенда
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

	log.Println("[Main] Сервер WebSocket с Redis persistence слушает на :8080/ws")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
