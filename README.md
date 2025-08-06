# 🗨️ Real-time чат-сервер с Redis и WebSocket

Это чат-сервер на WebSocket, написанный на Go, с поддержкой:

* Нескольких комнат
* Хранения сообщений в Redis
* Аутентификации через JWT
* REST API для регистрации и входа
* Docker + NGINX для развертывания

---

## 🚀 Фичи

* 🔐 **Аутентификация пользователей:** Безопасная регистрация и вход с использованием JWT и bcrypt
* 💬 **Чат через WebSocket:** Поддержка нескольких комнат и статуса «печатает»
* 📦 **Интеграция с Redis:**

  * Хранение истории сообщений
  * Рассылка сообщений через Pub/Sub
* 🔁 **Поддержание соединения:** Ping/Pong для контроля активности соединения
* 🐳 **Docker:** Готово к развёртыванию через Docker Compose и NGINX

---

## 📁 Структура проекта

```bash
.
├── main.go              # Серверная логика на Go
├── index.html           # Главная HTML-страница
├── static/              # Статические файлы
├── nginx.conf           # Конфигурация NGINX
├── Dockerfile           # Dockerfile приложения
├── docker-compose.yml   # Docker Compose файл
└── README.md            
```

---

## 🛠️ Инструкция по запуску

### 1. Требования

* [Docker](https://www.docker.com/)
* [Docker Compose](https://docs.docker.com/compose/)

---

### 2. Клонируйте и запустите

```bash
git clone https://github.com/shardy678/rt-chat.git
cd repo
docker-compose up --build --scale chat=3
```

Приложение будет доступно по адресу:
📍 `http://localhost`

---

## 🧪 API

### 🔐 Аутентификация

#### `POST /register`

Регистрация нового пользователя.

**Пример запроса:**

```json
{
  "user": "alice",
  "password": "securepassword"
}
```

**Ответ:**

```json
{"status": "registered"}
```

---

#### `POST /login`

Вход в систему. Возвращает JWT и устанавливает его в куки.

**Пример запроса:**

```json
{
  "user": "alice",
  "password": "securepassword"
}
```

**Ответ:**

```json
{
  "token": "..."
}
```

Устанавливается куки `token`, который используется WebSocket-соединением.

---

## 🔌 WebSocket

### URL:

```
ws://localhost/ws?room=general
```

**Требуется:**

* Куки `token` (выдаётся при входе)

**Формат отправляемого сообщения:**

```json
{
  "text": "Привет всем",
  "typing": false
}
```

**Сервер автоматически добавляет следующие поля:**

```json
{
  "user": "alice",
  "room": "general"
}
```

---

## 💾 Ключи Redis

* `users` → хэш пользователей и хэшей паролей
* `chat_history:{room}` → список сообщений комнаты
* `room:{room}` → канал Pub/Sub для трансляции сообщений

---

## ⚙️ Конфигурация

### В `.env` или `docker-compose.yml`:

```yaml
environment:
  - REDIS_ADDR=redis:6379
```

---

## 🌐 NGINX-прокси

Обеспечивает:

* Обслуживание статики
* Проксирование HTTP и WebSocket-запросов
* Заголовки Upgrade для WebSocket

---

## 📦 Docker-сервисы

| Сервис | Назначение             | Порт |
| ------ | ---------------------- | ---- |
| chat   | Go-сервер приложения   | 8080 |
| redis  | Redis-хранилище        | 6379 |
| nginx  | Обратный прокси-сервер | 80   |


---

## 🧱 Технологии

* **Go**
* **WebSocket**
* **Redis (Pub/Sub + хранение)**
* **JWT (аутентификация)**
* **bcrypt (хэширование паролей)**
* **Docker**
* **NGINX**
