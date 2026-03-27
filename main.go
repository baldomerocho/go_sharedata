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
	dbHost     = "localhost"
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

type Message struct {
	ID        int       `json:"id"`
	Username  string    `json:"username"`
	Content   string    `json:"content"`
	HasFile   bool      `json:"has_file"`
	FileName  string    `json:"file_name,omitempty"`
	FileSize  int64     `json:"file_size,omitempty"`
	FileData  string    `json:"file_data,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type WSEvent struct {
	Type    string   `json:"type"`
	Message *Message `json:"message,omitempty"`
	Period  string   `json:"period,omitempty"`
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
	http.HandleFunc("/api/messages", handleMessages)
	http.HandleFunc("/api/messages/delete", handleDeleteMessages)
	http.HandleFunc("/api/messages/delete/", handleDeleteSingleMessage)
	http.Handle("/", http.FileServer(http.FS(staticFiles)))

	tlsCert, err := generateSelfSignedCert()
	if err != nil {
		log.Fatal("Error generando certificado TLS:", err)
	}

	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{tlsCert},
	}

	server := &http.Server{
		Addr:      serverPort,
		TLSConfig: tlsConfig,
	}

	log.Printf("ShareData corriendo en https://0.0.0.0%s", serverPort)
	log.Fatal(server.ListenAndServeTLS("", ""))
}

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("Error upgrade WS:", err)
		return
	}
	defer conn.Close()

	conn.SetReadLimit(50 * 1024 * 1024) // 50MB max message

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

			saved, err := saveMessage(msg)
			if err != nil {
				log.Println("Error guardando mensaje:", err)
				continue
			}

			broadcast(WSEvent{Type: "new_message", Message: saved})

		case "delete_messages":
			if event.Period != "" {
				count, err := deleteMessages(event.Period)
				if err != nil {
					log.Println("Error borrando mensajes:", err)
					continue
				}
				log.Printf("Borrados %d mensajes (periodo: %s)", count, event.Period)
				broadcast(WSEvent{Type: "messages_deleted", Period: event.Period})
			}
		}
	}
}

func handleMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "Method not allowed", 405)
		return
	}

	limit := 100
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}

	offset := 0
	if o := r.URL.Query().Get("offset"); o != "" {
		if n, err := strconv.Atoi(o); err == nil && n >= 0 {
			offset = n
		}
	}

	rows, err := db.Query(`
		SELECT id, username, content, has_file, COALESCE(file_name,''), COALESCE(file_size,0), COALESCE(file_data,''), created_at
		FROM messages ORDER BY created_at ASC LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer rows.Close()

	messages := []Message{}
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.Username, &m.Content, &m.HasFile, &m.FileName, &m.FileSize, &m.FileData, &m.CreatedAt); err != nil {
			continue
		}
		messages = append(messages, m)
	}

	// Get total count
	var total int
	db.QueryRow("SELECT COUNT(*) FROM messages").Scan(&total)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"messages": messages,
		"total":    total,
	})
}

func handleDeleteMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", 405)
		return
	}

	var req struct {
		Period string `json:"period"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Bad request", 400)
		return
	}

	count, err := deleteMessages(req.Period)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	broadcast(WSEvent{Type: "messages_deleted", Period: req.Period})

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

	_, err = db.Exec("DELETE FROM messages WHERE id = $1", id)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	broadcast(WSEvent{Type: "message_removed", Message: &Message{ID: id}})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func saveMessage(msg *Message) (*Message, error) {
	var saved Message
	err := db.QueryRow(`
		INSERT INTO messages (username, content, has_file, file_name, file_size, file_data)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, username, content, has_file, COALESCE(file_name,''), COALESCE(file_size,0), COALESCE(file_data,''), created_at`,
		msg.Username, msg.Content, msg.HasFile, msg.FileName, msg.FileSize, msg.FileData,
	).Scan(&saved.ID, &saved.Username, &saved.Content, &saved.HasFile, &saved.FileName, &saved.FileSize, &saved.FileData, &saved.CreatedAt)
	return &saved, err
}

func deleteMessages(period string) (int64, error) {
	var query string
	switch period {
	case "today":
		query = "DELETE FROM messages WHERE created_at >= CURRENT_DATE"
	case "week":
		query = "DELETE FROM messages WHERE created_at >= date_trunc('week', CURRENT_DATE)"
	case "month":
		query = "DELETE FROM messages WHERE created_at >= date_trunc('month', CURRENT_DATE)"
	case "year":
		query = "DELETE FROM messages WHERE created_at >= date_trunc('year', CURRENT_DATE)"
	case "all":
		query = "DELETE FROM messages"
	default:
		return 0, fmt.Errorf("periodo no válido: %s", period)
	}
	res, err := db.Exec(query)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
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
