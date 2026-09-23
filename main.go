package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"embed"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io/fs"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	_ "github.com/lib/pq"
)

//go:embed static/*
var staticFiles embed.FS

const (
	dbHost      = "postgres-db"
	dbPort      = 5432
	dbUser      = "root"
	dbPassword  = "root"
	dbName      = "sharedata"
	serverPort  = ":8844"
	otpTTL      = 5 * time.Minute
	otpCooldown = 60 * time.Second // espera mínima entre dos envíos al mismo teléfono
	otpMaxTries = 5                // intentos fallidos antes de anular el código
	jwtTTL      = 30 * 24 * time.Hour
)

var jwtSecret = []byte(getenv("JWT_SECRET", "sharedata-secret-change-me-in-prod-9f3ac7"))

// Gateway de envío del OTP. Mismo contrato que GATEWAY_URL / X_API_KEY de
// go_otp_verify: POST {number, body, channel} con cabecera x-api-key y
// respuesta 200. Sirve tanto otp-show (sandbox) como el webhook de SMS.
var (
	otpURL     = getenv("OTP_URL", "http://otp-show-app-1:5066/api/otps")
	otpAPIKey  = os.Getenv("OTP_API_KEY")
	otpChannel = getenv("OTP_CHANNEL", "1")
)

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

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
	Blurhash  string    `json:"blurhash,omitempty"` // cifrado, como el resto del contenido
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

	if err := migrate(); err != nil {
		log.Fatal("Error ejecutando migraciones:", err)
	}

	http.HandleFunc("/api/auth/login", handleLogin)
	http.HandleFunc("/api/auth/verify", handleVerify)
	http.HandleFunc("/api/auth/me", authMW(handleMe))

	http.HandleFunc("/ws", handleWebSocket)
	http.HandleFunc("/api/channels", authMW(handleChannels))
	http.HandleFunc("/api/channels/", authMW(handleChannelAction))
	http.HandleFunc("/api/messages", authMW(handleMessages))
	http.HandleFunc("/api/files/", authMW(handleFile))
	http.HandleFunc("/api/messages/delete", authMW(handleDeleteMessages))
	http.HandleFunc("/api/messages/delete/", authMW(handleDeleteSingleMessage))
	staticFS, err := fs.Sub(staticFiles, "static")
	if err != nil {
		log.Fatal("Error montando static:", err)
	}
	http.Handle("/", http.FileServer(http.FS(staticFS)))

	useTLS := strings.EqualFold(getenv("TLS", "true"), "true")

	server := &http.Server{Addr: serverPort}

	if useTLS {
		tlsCert, err := generateSelfSignedCert()
		if err != nil {
			log.Fatal("Error generando certificado TLS:", err)
		}
		server.TLSConfig = &tls.Config{Certificates: []tls.Certificate{tlsCert}}
		log.Printf("ShareData corriendo en https://0.0.0.0%s", serverPort)
		log.Fatal(server.ListenAndServeTLS("", ""))
	} else {
		log.Printf("ShareData corriendo en http://0.0.0.0%s (TLS desactivado — detrás de proxy)", serverPort)
		log.Fatal(server.ListenAndServe())
	}
}

// ── WebSocket ──

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if token == "" {
		http.Error(w, "Unauthorized", 401)
		return
	}
	if _, err := verifyJWT(token); err != nil {
		http.Error(w, "Unauthorized", 401)
		return
	}

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
			// Se retransmiten solo los metadatos: reenviar el adjunto entero a
			// cada cliente conectado es justo lo que hacía lenta la app. Quien
			// lo necesite lo pide a /api/files/{id}.
			meta := *saved
			meta.FileData = ""
			broadcast(WSEvent{Type: "new_message", Message: &meta})

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
		// Los mensajes (y con ellos sus adjuntos, que viven en file_data) se
		// borran explícitamente en la misma transacción: no se depende de que
		// el esquema tenga ON DELETE CASCADE.
		tx, err := db.Begin()
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer tx.Rollback()
		if _, err := tx.Exec("DELETE FROM messages WHERE channel_id = $1", id); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		res, err := tx.Exec("DELETE FROM channels WHERE id = $1", id)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			http.Error(w, "Canal no encontrado", 404)
			return
		}
		if err := tx.Commit(); err != nil {
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

	limit := 50
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}

	// before = id del mensaje más antiguo ya cargado por el cliente.
	// Ausente o 0 → primera página, es decir los mensajes más recientes.
	before := 0
	if b := r.URL.Query().Get("before"); b != "" {
		if n, err := strconv.Atoi(b); err == nil && n > 0 {
			before = n
		}
	}

	// file_data queda fuera a propósito: se sirve aparte en /api/files/{id}.
	// Pedimos un registro de más para saber si quedan páginas anteriores.
	rows, err := db.Query(`
		SELECT id, channel_id, username, content, has_file, COALESCE(file_name,''), COALESCE(file_size,0), COALESCE(blurhash,''), created_at
		FROM messages
		WHERE channel_id = $1 AND ($2 = 0 OR id < $2)
		ORDER BY id DESC LIMIT $3`, channelID, before, limit+1)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer rows.Close()

	messages := []Message{}
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.ChannelID, &m.Username, &m.Content, &m.HasFile, &m.FileName, &m.FileSize, &m.Blurhash, &m.CreatedAt); err != nil {
			continue
		}
		messages = append(messages, m)
	}

	hasMore := len(messages) > limit
	if hasMore {
		messages = messages[:limit]
	}

	// La consulta viene descendente (más recientes primero); el cliente los
	// pinta en orden cronológico, así que se invierte aquí.
	for i, j := 0, len(messages)-1; i < j; i, j = i+1, j-1 {
		messages[i], messages[j] = messages[j], messages[i]
	}

	var total int
	db.QueryRow("SELECT COUNT(*) FROM messages WHERE channel_id = $1", channelID).Scan(&total)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"messages": messages, "total": total, "has_more": hasMore})
}

// handleFile sirve el adjunto cifrado de un mensaje. Se separa de /api/messages
// para que el historial cargue sin arrastrar megabytes de base64 por mensaje.
func handleFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "Method not allowed", 405)
		return
	}

	idStr := r.URL.Path[len("/api/files/"):]
	id, err := strconv.Atoi(idStr)
	if err != nil || id <= 0 {
		http.Error(w, "Invalid ID", 400)
		return
	}

	var fileName, fileData string
	err = db.QueryRow(`
		SELECT COALESCE(file_name,''), COALESCE(file_data,'')
		FROM messages WHERE id = $1`, id).Scan(&fileName, &fileData)
	if err == sql.ErrNoRows {
		http.Error(w, "No encontrado", 404)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if fileData == "" {
		http.Error(w, "El mensaje no tiene adjunto", 404)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"id": id, "file_name": fileName, "file_data": fileData})
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
		INSERT INTO messages (channel_id, username, content, has_file, file_name, file_size, file_data, blurhash)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id, channel_id, username, content, has_file, COALESCE(file_name,''), COALESCE(file_size,0), COALESCE(file_data,''), COALESCE(blurhash,''), created_at`,
		msg.ChannelID, msg.Username, msg.Content, msg.HasFile, msg.FileName, msg.FileSize, msg.FileData, msg.Blurhash,
	).Scan(&saved.ID, &saved.ChannelID, &saved.Username, &saved.Content, &saved.HasFile, &saved.FileName, &saved.FileSize, &saved.FileData, &saved.Blurhash, &saved.CreatedAt)
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

// ── Auth / OTP / JWT ──

func migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS users (
			id SERIAL PRIMARY KEY,
			phone VARCHAR(30) UNIQUE NOT NULL,
			name VARCHAR(100),
			created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS otps (
			id SERIAL PRIMARY KEY,
			phone VARCHAR(30) NOT NULL,
			code VARCHAR(6) NOT NULL,
			expires_at TIMESTAMP WITH TIME ZONE NOT NULL,
			used BOOLEAN DEFAULT FALSE,
			created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_otps_phone ON otps(phone, created_at DESC)`,
		`CREATE TABLE IF NOT EXISTS channels (
			id SERIAL PRIMARY KEY,
			name VARCHAR(200) NOT NULL DEFAULT 'General',
			created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
		)`,
		// Canal 1 ("General") en una base de datos vacía; si ya hay canales no toca nada.
		`INSERT INTO channels (name) SELECT 'General' WHERE NOT EXISTS (SELECT 1 FROM channels)`,
		`CREATE TABLE IF NOT EXISTS messages (
			id SERIAL PRIMARY KEY,
			username VARCHAR(100) NOT NULL DEFAULT 'Anónimo',
			content TEXT NOT NULL DEFAULT '',
			has_file BOOLEAN DEFAULT FALSE,
			file_name VARCHAR(500),
			file_size BIGINT DEFAULT 0,
			file_data TEXT,
			created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
			channel_id INTEGER DEFAULT 1 REFERENCES channels(id) ON DELETE CASCADE,
			blurhash TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_channel ON messages(channel_id, created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_created_at ON messages(created_at DESC)`,
		`ALTER TABLE otps ADD COLUMN IF NOT EXISTS attempts INT NOT NULL DEFAULT 0`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return err
		}
	}

	// Bases de datos creadas antes de que existiera la columna del blurhash.
	if _, err := db.Exec(`ALTER TABLE messages ADD COLUMN IF NOT EXISTS blurhash TEXT`); err != nil {
		log.Println("Aviso: no se pudo añadir la columna blurhash:", err)
	}
	var n int
	db.QueryRow("SELECT COUNT(*) FROM users").Scan(&n)
	if n == 0 {
		log.Println("⚠  No hay usuarios registrados. Registra uno con:")
		log.Println("    INSERT INTO users (phone, name) VALUES ('+593XXXXXXXXX', 'Nombre');")
	}
	return nil
}

func signJWT(phone string) (string, error) {
	header := map[string]string{"alg": "HS256", "typ": "JWT"}
	claims := map[string]any{
		"phone": phone,
		"iat":   time.Now().Unix(),
		"exp":   time.Now().Add(jwtTTL).Unix(),
	}
	hb, _ := json.Marshal(header)
	cb, _ := json.Marshal(claims)
	h := base64.RawURLEncoding.EncodeToString(hb)
	c := base64.RawURLEncoding.EncodeToString(cb)
	unsigned := h + "." + c
	mac := hmac.New(sha256.New, jwtSecret)
	mac.Write([]byte(unsigned))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return unsigned + "." + sig, nil
}

func verifyJWT(token string) (map[string]any, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("token inválido")
	}
	mac := hmac.New(sha256.New, jwtSecret)
	mac.Write([]byte(parts[0] + "." + parts[1]))
	expected := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expected), []byte(parts[2])) {
		return nil, fmt.Errorf("firma inválida")
	}
	cb, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, err
	}
	var claims map[string]any
	if err := json.Unmarshal(cb, &claims); err != nil {
		return nil, err
	}
	if exp, ok := claims["exp"].(float64); ok {
		if time.Now().Unix() > int64(exp) {
			return nil, fmt.Errorf("expirado")
		}
	}
	return claims, nil
}

func authMW(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var token string
		if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
			token = h[7:]
		}
		if token == "" {
			token = r.URL.Query().Get("token")
		}
		if token == "" {
			http.Error(w, "Unauthorized", 401)
			return
		}
		claims, err := verifyJWT(token)
		if err != nil {
			http.Error(w, "Unauthorized", 401)
			return
		}
		r.Header.Set("X-Phone", fmt.Sprint(claims["phone"]))
		next(w, r)
	}
}

func sendOTP(code, phone string) error {
	body, _ := json.Marshal(map[string]string{
		"number":  phone,
		"body":    code,
		"channel": otpChannel,
	})
	req, err := http.NewRequest("POST", otpURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if otpAPIKey != "" {
		req.Header.Set("x-api-key", otpAPIKey)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("otp service respondió %d", resp.StatusCode)
	}
	return nil
}

// newOTPCode genera 6 dígitos con crypto/rand.
func newOTPCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1000000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

// reserveOTP comprueba el enfriamiento por teléfono e inserta el código nuevo.
// El advisory lock serializa logins simultáneos del mismo teléfono, para que
// dos peticiones en paralelo no se salten el enfriamiento. Devuelve 0 y la
// espera restante si todavía no se puede enviar otro.
func reserveOTP(phone, code string) (int, time.Duration, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("SELECT pg_advisory_xact_lock(hashtext($1))", phone); err != nil {
		return 0, 0, err
	}
	var since float64
	err = tx.QueryRow(`SELECT EXTRACT(EPOCH FROM NOW() - created_at) FROM otps
		WHERE phone = $1 ORDER BY created_at DESC LIMIT 1`, phone).Scan(&since)
	if err != nil && err != sql.ErrNoRows {
		return 0, 0, err
	}
	if err == nil {
		if wait := otpCooldown - time.Duration(since*float64(time.Second)); wait > 0 {
			return 0, wait, nil
		}
	}
	var id int
	if err := tx.QueryRow("INSERT INTO otps (phone, code, expires_at) VALUES ($1, $2, $3) RETURNING id",
		phone, code, time.Now().Add(otpTTL)).Scan(&id); err != nil {
		return 0, 0, err
	}
	return id, 0, tx.Commit()
}

func handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", 405)
		return
	}
	var req struct {
		Phone string `json:"phone"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Bad request", 400)
		return
	}
	phone := strings.TrimSpace(req.Phone)
	if phone == "" {
		http.Error(w, "Teléfono requerido", 400)
		return
	}

	var exists bool
	if err := db.QueryRow("SELECT EXISTS(SELECT 1 FROM users WHERE phone = $1)", phone).Scan(&exists); err != nil {
		http.Error(w, "Error interno", 500)
		return
	}
	if !exists {
		http.Error(w, "Número no registrado", 403)
		return
	}

	code, err := newOTPCode()
	if err != nil {
		http.Error(w, "Error interno", 500)
		return
	}
	id, wait, err := reserveOTP(phone, code)
	if err != nil {
		http.Error(w, "Error interno", 500)
		return
	}
	if wait > 0 {
		secs := int(wait.Seconds() + 0.999)
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		http.Error(w, fmt.Sprintf("Espera %d s antes de pedir otro código", secs), 429)
		return
	}

	if err := sendOTP(code, phone); err != nil {
		log.Println("Error enviando OTP:", err)
		// El código nunca llegó: se borra para no bloquear el reintento.
		db.Exec("DELETE FROM otps WHERE id = $1", id)
		http.Error(w, "No se pudo enviar el código", 502)
		return
	}
	// Solo el último código enviado es válido.
	db.Exec("UPDATE otps SET used = true WHERE phone = $1 AND id <> $2 AND used = false", phone, id)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "ttl": int(otpTTL.Seconds())})
}

func handleVerify(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", 405)
		return
	}
	var req struct {
		Phone string `json:"phone"`
		Code  string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Bad request", 400)
		return
	}
	phone := strings.TrimSpace(req.Phone)
	code := strings.TrimSpace(req.Code)
	if phone == "" || len(code) != 6 {
		http.Error(w, "Datos inválidos", 400)
		return
	}

	// Cada intento consume uno del OTP vigente, acierte o no. Un único UPDATE
	// atómico: el FOR UPDATE impide que dos peticiones canjeen el mismo código,
	// y al acertar o agotar los intentos el código queda anulado.
	var ok bool
	err := db.QueryRow(`
		UPDATE otps SET
			attempts = attempts + 1,
			used = (code = $2 OR attempts + 1 >= $3)
		WHERE id = (
			SELECT id FROM otps
			WHERE phone = $1 AND used = false AND expires_at > NOW()
			ORDER BY created_at DESC LIMIT 1
			FOR UPDATE
		) AND attempts < $3
		RETURNING code = $2`, phone, code, otpMaxTries).Scan(&ok)
	if err != nil && err != sql.ErrNoRows {
		http.Error(w, "Error interno", 500)
		return
	}
	if !ok {
		http.Error(w, "Código inválido o expirado", 401)
		return
	}
	db.Exec("DELETE FROM otps WHERE expires_at < NOW() - INTERVAL '1 day'")

	token, err := signJWT(phone)
	if err != nil {
		http.Error(w, "Error firmando token", 500)
		return
	}

	var name sql.NullString
	db.QueryRow("SELECT name FROM users WHERE phone = $1", phone).Scan(&name)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"token": token,
		"phone": phone,
		"name":  name.String,
	})
}

func handleMe(w http.ResponseWriter, r *http.Request) {
	phone := r.Header.Get("X-Phone")
	var name sql.NullString
	db.QueryRow("SELECT name FROM users WHERE phone = $1", phone).Scan(&name)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"phone": phone, "name": name.String})
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
