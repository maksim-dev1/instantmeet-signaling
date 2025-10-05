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
}

// Структура для комнаты
type Room struct {
	ID      string
	Clients map[string]*Client
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

// Получение или создание комнаты
func getOrCreateRoom(roomID string) *Room {
	roomsMu.Lock()
	defer roomsMu.Unlock()

	if room, exists := rooms[roomID]; exists {
		return room
	}

	room := &Room{
		ID:      roomID,
		Clients: make(map[string]*Client),
	}
	rooms[roomID] = room
	log.Printf("Создана новая комната: %s", roomID)
	return room
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
	}

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
	client.RoomID = msg.RoomID
	client.Username = msg.Username

	room := getOrCreateRoom(msg.RoomID)
	room.mu.Lock()
	room.Clients[client.ID] = client
	room.mu.Unlock()

	log.Printf("Клиент %s (%s) присоединился к комнате %s", client.ID, client.Username, client.RoomID)

	// Отправляем подтверждение клиенту
	response := Message{
		Type:   "joined",
		RoomID: client.RoomID,
	}
	sendMessage(client, response)

	// Уведомляем других участников комнаты
	notifyOthersInRoom(client, Message{
		Type:     "user-joined",
		From:     client.ID,
		Username: client.Username,
		RoomID:   client.RoomID,
	})
}

// Обработка signaling сообщений (offer, answer, ice-candidate)
func handleSignaling(client *Client, msg Message) {
	if msg.To == "" {
		log.Printf("Сообщение без получателя")
		return
	}

	room := rooms[client.RoomID]
	if room == nil {
		log.Printf("Комната не найдена: %s", client.RoomID)
		return
	}

	room.mu.Lock()
	targetClient, exists := room.Clients[msg.To]
	room.mu.Unlock()

	if !exists {
		log.Printf("Получатель не найден: %s", msg.To)
		return
	}

	// Добавляем информацию об отправителе
	msg.From = client.ID

	log.Printf("Перенаправление сообщения типа %s от %s к %s", msg.Type, msg.From, msg.To)
	sendMessage(targetClient, msg)
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

	room := rooms[client.RoomID]
	if room == nil {
		return
	}

	room.mu.Lock()
	delete(room.Clients, client.ID)
	clientCount := len(room.Clients)
	room.mu.Unlock()

	log.Printf("Клиент %s покинул комнату %s", client.ID, client.RoomID)

	// Уведомляем других участников
	notifyOthersInRoom(client, Message{
		Type:   "user-left",
		From:   client.ID,
		RoomID: client.RoomID,
	})

	// Удаляем пустую комнату
	if clientCount == 0 {
		roomsMu.Lock()
		delete(rooms, client.RoomID)
		roomsMu.Unlock()
		log.Printf("Комната %s удалена (пустая)", client.RoomID)
	}
}

// Отправка сообщения клиенту
func sendMessage(client *Client, msg Message) {
	data, err := json.Marshal(msg)
	if err != nil {
		log.Printf("Ошибка сериализации сообщения: %v", err)
		return
	}

	if err := client.Conn.WriteMessage(websocket.TextMessage, data); err != nil {
		log.Printf("Ошибка отправки сообщения: %v", err)
	}
}

// Уведомление других участников комнаты
func notifyOthersInRoom(sender *Client, msg Message) {
	room := rooms[sender.RoomID]
	if room == nil {
		return
	}

	room.mu.Lock()
	defer room.mu.Unlock()

	for clientID, client := range room.Clients {
		if clientID != sender.ID {
			sendMessage(client, msg)
		}
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
