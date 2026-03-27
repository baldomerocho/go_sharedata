package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"embed"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	_ "github.com/lib/pq"
)

//go:embed static/*
var staticFiles embed.FS

const (
	dbHost     = "postgres-db"
	dbPort     = 5432
	dbUser     = "root"
	dbPassword = "root"
	dbName     = "sharedata"
	serverPort = ":8844"
)

var (
	db       *sql.DB
	upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
		ReadBufferSize:  1024 * 64,
		WriteBufferSize: 1024 * 64,
	}
	clients   = make(map[*websocket.Conn]bool)
	clientsMu sync.RWMutex
)

type Channel struct {
	ID        int       `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

type Message struct {
	ID        int       `json:"id"`
	ChannelID int       `json:"channel_id"`
	Username  string    `json:"username"`
	Content   string    `json:"content"`
	HasFile   bool      `json:"has_file"`
	FileName  string    `json:"file_name,omitempty"`
	FileSize  int64     `json:"file_size,omitempty"`
	FileData  string    `json:"file_data,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type WSEvent struct {
	Type      string   `json:"type"`
	Message   *Message `json:"message,omitempty"`
	Channel   *Channel `json:"channel,omitempty"`
	ChannelID int      `json:"channel_id,omitempty"`
	Period    string   `json:"period,omitempty"`
}

func main() {
	var err error
	connStr := fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=disable",
		dbHost, dbPort, dbUser, dbPassword, dbName)

	db, err = sql.Open("postgres", connStr)
	if err != nil {
		log.Fatal("Error conectando a PostgreSQL:", err)
	}
	defer db.Close()

	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(5)

	if err = db.Ping(); err != nil {
		log.Fatal("No se puede hacer ping a PostgreSQL:", err)
	}
	log.Println("Conectado a PostgreSQL")

	http.HandleFunc("/ws", handleWebSocket)
	http.HandleFunc("/api/channels", handleChannels)
	http.HandleFunc("/api/channels/", handleChannelAction)
	http.HandleFunc("/api/messages", handleMessages)
	http.HandleFunc("/api/messages/delete", handleDeleteMessages)
	http.HandleFunc("/api/messages/delete/", handleDeleteSingleMessage)
	http.Handle("/", http.FileServer(http.FS(staticFiles)))

	tlsCert, err := generateSelfSignedCert()
	if err != nil {
		log.Fatal("Error generando certificado TLS:", err)
	}

	server := &http.Server{
		Addr:      serverPort,
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{tlsCert}},
	}

	log.Printf("ShareData corriendo en https://0.0.0.0%s", serverPort)
	log.Fatal(server.ListenAndServeTLS("", ""))
}

// ── WebSocket ──

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("Error upgrade WS:", err)
		return
	}
	defer conn.Close()

	conn.SetReadLimit(50 * 1024 * 1024)

	clientsMu.Lock()
	clients[conn] = true
	clientsMu.Unlock()

	defer func() {
		clientsMu.Lock()
		delete(clients, conn)
		clientsMu.Unlock()
	}()

	for {
		_, msgBytes, err := conn.ReadMessage()
		if err != nil {
			break
		}

		var event WSEvent
		if err := json.Unmarshal(msgBytes, &event); err != nil {
			continue
		}

		switch event.Type {
		case "new_message":
			if event.Message == nil {
				continue
			}
			msg := event.Message
			if msg.Content == "" && !msg.HasFile {
				continue
			}
			if msg.Username == "" {
				msg.Username = "Anónimo"
			}
			if msg.ChannelID == 0 {
				msg.ChannelID = 1
			}

			saved, err := saveMessage(msg)
			if err != nil {
				log.Println("Error guardando mensaje:", err)
				continue
			}
			broadcast(WSEvent{Type: "new_message", Message: saved})

		case "delete_messages":
			if event.Period != "" {
				chID := event.ChannelID
				if chID == 0 {
					chID = 1
				}
				count, err := deleteMessages(event.Period, chID)
				if err != nil {
					log.Println("Error borrando mensajes:", err)
					continue
				}
				log.Printf("Borrados %d mensajes (canal: %d, periodo: %s)", count, chID, event.Period)
				broadcast(WSEvent{Type: "messages_deleted", ChannelID: chID, Period: event.Period})
			}
		}
	}
}

func broadcast(event WSEvent) {
	data, err := json.Marshal(event)
	if err != nil {
		return
	}
	clientsMu.RLock()
	defer clientsMu.RUnlock()
	for conn := range clients {
		if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
			conn.Close()
			delete(clients, conn)
		}
	}
}

// ── Channels ──

func handleChannels(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		rows, err := db.Query("SELECT id, name, created_at FROM channels ORDER BY created_at ASC")
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer rows.Close()

		channels := []Channel{}
		for rows.Next() {
			var c Channel
			rows.Scan(&c.ID, &c.Name, &c.CreatedAt)
			channels = append(channels, c)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(channels)

	case "POST":
		var req struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
			http.Error(w, "Bad request", 400)
			return
		}
		var ch Channel
		err := db.QueryRow("INSERT INTO channels (name) VALUES ($1) RETURNING id, name, created_at", req.Name).
			Scan(&ch.ID, &ch.Name, &ch.CreatedAt)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		broadcast(WSEvent{Type: "channel_created", Channel: &ch})
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(ch)

	default:
		http.Error(w, "Method not allowed", 405)
	}
}

func handleChannelAction(w http.ResponseWriter, r *http.Request) {
	idStr := r.URL.Path[len("/api/channels/"):]
	id, err := strconv.Atoi(idStr)
	if err != nil {
		http.Error(w, "Invalid ID", 400)
		return
	}

	switch r.Method {
	case "PUT":
		var req struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
			http.Error(w, "Bad request", 400)
			return
		}
		_, err := db.Exec("UPDATE channels SET name = $1 WHERE id = $2", req.Name, id)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		broadcast(WSEvent{Type: "channel_updated", Channel: &Channel{ID: id, Name: req.Name}})
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true})

	case "DELETE":
		if id == 1 {
			http.Error(w, "No se puede eliminar el canal General", 400)
			return
		}
		_, err := db.Exec("DELETE FROM channels WHERE id = $1", id)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		go db.Exec("VACUUM messages")
		broadcast(WSEvent{Type: "channel_deleted", ChannelID: id})
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true})

	default:
		http.Error(w, "Method not allowed", 405)
	}
}

// ── Messages ──

func handleMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "Method not allowed", 405)
		return
	}

	channelID := 1
	if c := r.URL.Query().Get("channel_id"); c != "" {
		if n, err := strconv.Atoi(c); err == nil && n > 0 {
			channelID = n
		}
	}

	limit := 200
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}

	rows, err := db.Query(`
		SELECT id, channel_id, username, content, has_file, COALESCE(file_name,''), COALESCE(file_size,0), COALESCE(file_data,''), created_at
		FROM messages WHERE channel_id = $1 ORDER BY created_at ASC LIMIT $2`, channelID, limit)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer rows.Close()

	messages := []Message{}
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.ChannelID, &m.Username, &m.Content, &m.HasFile, &m.FileName, &m.FileSize, &m.FileData, &m.CreatedAt); err != nil {
			continue
		}
		messages = append(messages, m)
	}

	var total int
	db.QueryRow("SELECT COUNT(*) FROM messages WHERE channel_id = $1", channelID).Scan(&total)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"messages": messages, "total": total})
}

func handleDeleteMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", 405)
		return
	}

	var req struct {
		Period    string `json:"period"`
		ChannelID int    `json:"channel_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Bad request", 400)
		return
	}
	if req.ChannelID == 0 {
		req.ChannelID = 1
	}

	count, err := deleteMessages(req.Period, req.ChannelID)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	broadcast(WSEvent{Type: "messages_deleted", ChannelID: req.ChannelID, Period: req.Period})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"deleted": count})
}

func handleDeleteSingleMessage(w http.ResponseWriter, r *http.Request) {
	if r.Method != "DELETE" {
		http.Error(w, "Method not allowed", 405)
		return
	}

	idStr := r.URL.Path[len("/api/messages/delete/"):]
	id, err := strconv.Atoi(idStr)
	if err != nil {
		http.Error(w, "Invalid ID", 400)
		return
	}

	var channelID int
	db.QueryRow("SELECT channel_id FROM messages WHERE id = $1", id).Scan(&channelID)

	_, err = db.Exec("DELETE FROM messages WHERE id = $1", id)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	broadcast(WSEvent{Type: "message_removed", Message: &Message{ID: id, ChannelID: channelID}})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// ── DB helpers ──

func saveMessage(msg *Message) (*Message, error) {
	var saved Message
	err := db.QueryRow(`
		INSERT INTO messages (channel_id, username, content, has_file, file_name, file_size, file_data)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id, channel_id, username, content, has_file, COALESCE(file_name,''), COALESCE(file_size,0), COALESCE(file_data,''), created_at`,
		msg.ChannelID, msg.Username, msg.Content, msg.HasFile, msg.FileName, msg.FileSize, msg.FileData,
	).Scan(&saved.ID, &saved.ChannelID, &saved.Username, &saved.Content, &saved.HasFile, &saved.FileName, &saved.FileSize, &saved.FileData, &saved.CreatedAt)
	return &saved, err
}

func deleteMessages(period string, channelID int) (int64, error) {
	var base string
	switch period {
	case "today":
		base = "DELETE FROM messages WHERE channel_id = $1 AND created_at >= CURRENT_DATE"
	case "week":
		base = "DELETE FROM messages WHERE channel_id = $1 AND created_at >= date_trunc('week', CURRENT_DATE)"
	case "month":
		base = "DELETE FROM messages WHERE channel_id = $1 AND created_at >= date_trunc('month', CURRENT_DATE)"
	case "year":
		base = "DELETE FROM messages WHERE channel_id = $1 AND created_at >= date_trunc('year', CURRENT_DATE)"
	case "all":
		base = "DELETE FROM messages WHERE channel_id = $1"
	default:
		return 0, fmt.Errorf("periodo no válido: %s", period)
	}
	res, err := db.Exec(base, channelID)
	if err != nil {
		return 0, err
	}
	count, _ := res.RowsAffected()
	if count > 0 {
		go db.Exec("VACUUM messages")
	}
	return count, nil
}

// ── TLS ──

func generateSelfSignedCert() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}

	serialNumber, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))

	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject:      pkix.Name{Organization: []string{"ShareData"}},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("0.0.0.0"), net.ParseIP("127.0.0.1"), net.ParseIP("192.168.0.8")},
		DNSNames:     []string{"localhost"},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyDER, _ := x509.MarshalECPrivateKey(key)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	return tls.X509KeyPair(certPEM, keyPEM)
}
