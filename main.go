package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"
)

// Структура для клиента
type Client struct {
	ID       string
	Conn     *websocket.Conn
	RoomID   string
	Username string
	Send     chan Message
	mu       sync.Mutex
}

// Структура для комнаты
type Room struct {
	ID      string
	Clients map[*Client]bool
	mu      sync.Mutex
}

// Структура сообщения
type Message struct {
	Type     string          `json:"type"`
	From     string          `json:"from,omitempty"`
	To       string          `json:"to,omitempty"`
	RoomID   string          `json:"roomId,omitempty"`
	Username string          `json:"username,omitempty"`
	Data     json.RawMessage `json:"data,omitempty"`
}

// Глобальные переменные
var (
	upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return true // Разрешаем все origins для разработки
		},
	}
	rooms   = make(map[string]*Room)
	roomsMu sync.Mutex
)

// Writer goroutine для клиента
func (c *Client) writePump() {
	defer func() {
		c.Conn.Close()
	}()

	for message := range c.Send {
		data, err := json.Marshal(message)
		if err != nil {
			log.Printf("Ошибка сериализации сообщения: %v", err)
			continue
		}

		if err := c.Conn.WriteMessage(websocket.TextMessage, data); err != nil {
			log.Printf("Ошибка отправки сообщения клиенту %s: %v", c.ID, err)
			return
		}
	}
}

// Обработчик WebSocket соединений
func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Ошибка upgrade соединения: %v", err)
		return
	}

	client := &Client{
		Conn: conn,
		Send: make(chan Message, 256),
	}

	// Запускаем горутину для отправки сообщений
	go client.writePump()

	log.Printf("Новое WebSocket соединение")

	// Чтение сообщений от клиента
	for {
		_, messageData, err := conn.ReadMessage()
		if err != nil {
			log.Printf("Ошибка чтения сообщения: %v", err)
			// Удаляем клиента из комнаты при разрыве соединения
			if client.RoomID != "" {
				removeClientFromRoom(client)
			}
			close(client.Send)
			conn.Close()
			break
		}

		var msg Message
		if err := json.Unmarshal(messageData, &msg); err != nil {
			log.Printf("Ошибка парсинга JSON: %v", err)
			continue
		}

		log.Printf("Получено сообщение типа: %s от клиента: %s", msg.Type, msg.From)

		// Обработка различных типов сообщений
		switch msg.Type {
		case "join":
			handleJoin(client, msg)
		case "offer":
			handleSignaling(client, msg)
		case "answer":
			handleSignaling(client, msg)
		case "ice-candidate":
			handleSignaling(client, msg)
		case "leave":
			handleLeave(client, msg)
		default:
			log.Printf("Неизвестный тип сообщения: %s", msg.Type)
		}
	}
}

// Обработка присоединения к комнате
func handleJoin(client *Client, msg Message) {
	client.ID = msg.From
	client.Username = msg.Username

	// Если roomId пустой, игнорируем это сообщение
	if msg.RoomID == "" {
		log.Printf("Получен join без roomId от клиента %s", client.ID)
		return
	}

	client.RoomID = msg.RoomID

	// Получаем или создаем комнату
	roomsMu.Lock()
	room, exists := rooms[msg.RoomID]
	if !exists {
		room = &Room{
			ID:      msg.RoomID,
			Clients: make(map[*Client]bool),
		}
		rooms[msg.RoomID] = room
		log.Printf("Создана новая комната: %s", msg.RoomID)
	}
	roomsMu.Unlock()

	// Добавляем клиента в комнату
	room.mu.Lock()
	room.Clients[client] = true

	// Уведомляем ВСЕХ других участников о новом пользователе
	for otherClient := range room.Clients {
		if otherClient != client {
			notification := Message{
				Type:     "user-joined",
				From:     client.ID,
				Username: client.Username,
				RoomID:   msg.RoomID,
			}
			otherClient.Send <- notification
			log.Printf("Отправлено уведомление user-joined клиенту %s о присоединении %s", otherClient.ID, client.ID)
		}
	}
	room.mu.Unlock()

	log.Printf("Клиент %s (%s) присоединился к комнате %s", client.ID, client.Username, client.RoomID)

	// Отправляем подтверждение клиенту
	response := Message{
		Type:   "joined",
		RoomID: client.RoomID,
	}
	client.Send <- response
}

// Обработка signaling сообщений (offer, answer, ice-candidate)
func handleSignaling(client *Client, msg Message) {
	if msg.To == "" {
		log.Printf("Сообщение без получателя")
		return
	}

	roomsMu.Lock()
	room := rooms[client.RoomID]
	roomsMu.Unlock()

	if room == nil {
		log.Printf("Комната не найдена: %s", client.RoomID)
		return
	}

	room.mu.Lock()
	defer room.mu.Unlock()

	// Ищем получателя в комнате
	var targetClient *Client
	for c := range room.Clients {
		if c.ID == msg.To {
			targetClient = c
			break
		}
	}

	if targetClient == nil {
		log.Printf("Получатель не найден: %s", msg.To)
		return
	}

	// Добавляем информацию об отправителе
	msg.From = client.ID

	log.Printf("Перенаправление сообщения типа %s от %s к %s", msg.Type, msg.From, msg.To)
	targetClient.Send <- msg
}

// Обработка выхода из комнаты
func handleLeave(client *Client, msg Message) {
	removeClientFromRoom(client)
}

// Удаление клиента из комнаты
func removeClientFromRoom(client *Client) {
	if client.RoomID == "" {
		return
	}

	roomsMu.Lock()
	room := rooms[client.RoomID]
	roomsMu.Unlock()

	if room == nil {
		return
	}

	room.mu.Lock()
	delete(room.Clients, client)
	clientCount := len(room.Clients)

	// Уведомляем других участников
	for otherClient := range room.Clients {
		notification := Message{
			Type:   "user-left",
			From:   client.ID,
			RoomID: client.RoomID,
		}
		otherClient.Send <- notification
	}
	room.mu.Unlock()

	log.Printf("Клиент %s покинул комнату %s", client.ID, client.RoomID)

	// Удаляем пустую комнату
	if clientCount == 0 {
		roomsMu.Lock()
		delete(rooms, client.RoomID)
		roomsMu.Unlock()
		log.Printf("Комната %s удалена (пустая)", client.RoomID)
	}
}

// Обработчик для корневого пути
func handleHome(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, "InstantMeet Signaling Server is running!")
}

func main() {
	// Роуты
	http.HandleFunc("/", handleHome)
	http.HandleFunc("/ws", handleWebSocket)

	// Запуск сервера
	port := ":3000"
	log.Printf("Signaling сервер запущен на порту %s", port)
	if err := http.ListenAndServe(port, nil); err != nil {
		log.Fatal("Ошибка запуска сервера: ", err)
	}
}
