package main

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Client struct {
	conn io.ReadWriteCloser
	send chan []byte
}

type ChatMessage struct {
	Sender string `json:"sender"`
	Text   string `json:"text"`
}

// Room holds per-room state: connected clients and panel grouping
type Room struct {
	mu                   sync.Mutex
	clients              map[*Client]bool
	currentPanelMessages []ChatMessage
	currentPanelID       string
	activeSpeakers       []string
	Seed                 int
}

var (
	rooms      = make(map[string]*Room)
	roomsMu    sync.Mutex
	httpClient = &http.Client{
		Timeout: 30 * time.Second,
	}
	maxMessageChars   = 500
	maxWSPayloadBytes = 4096
	limiter           *ipRateLimiter
	rateAllowedTotal  uint64
	rateDroppedTotal  uint64
)

type ipRateState struct {
	shortWindowStart time.Time
	shortCount       int
	longWindowStart  time.Time
	longCount        int
	lastSeen         time.Time
}

type ipRateLimiter struct {
	mu          sync.Mutex
	states      map[string]*ipRateState
	shortLimit  int
	shortWindow time.Duration
	longLimit   int
	longWindow  time.Duration
	stateTTL    time.Duration
}

func newIPRateLimiter(shortLimit int, shortWindow time.Duration, longLimit int, longWindow time.Duration, stateTTL time.Duration) *ipRateLimiter {
	return &ipRateLimiter{
		states:      make(map[string]*ipRateState),
		shortLimit:  shortLimit,
		shortWindow: shortWindow,
		longLimit:   longLimit,
		longWindow:  longWindow,
		stateTTL:    stateTTL,
	}
}

func (l *ipRateLimiter) allow(ip string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	state, ok := l.states[ip]
	if !ok {
		l.states[ip] = &ipRateState{
			shortWindowStart: now,
			shortCount:       1,
			longWindowStart:  now,
			longCount:        1,
			lastSeen:         now,
		}
		return true
	}

	if now.Sub(state.shortWindowStart) >= l.shortWindow {
		state.shortWindowStart = now
		state.shortCount = 0
	}
	if now.Sub(state.longWindowStart) >= l.longWindow {
		state.longWindowStart = now
		state.longCount = 0
	}

	if state.shortCount >= l.shortLimit || state.longCount >= l.longLimit {
		state.lastSeen = now
		return false
	}

	state.shortCount++
	state.longCount++
	state.lastSeen = now
	return true
}

func (l *ipRateLimiter) activeIPs(now time.Time) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	for ip, state := range l.states {
		if now.Sub(state.lastSeen) > l.stateTTL {
			delete(l.states, ip)
		}
	}
	return len(l.states)
}

func getOrCreateRoom(roomID string) *Room {
	roomsMu.Lock()
	defer roomsMu.Unlock()
	if r, ok := rooms[roomID]; ok {
		return r
	}
	r := &Room{
		clients: make(map[*Client]bool),
		Seed:    rand.Intn(1000000), // Stable seed for character design consistency
	}
	rooms[roomID] = r
	return r
}

func loadEnv() {
	file, err := os.Open(".env")
	if err != nil {
		return
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			key := strings.TrimSpace(parts[0])
			val := strings.TrimSpace(parts[1])
			val = strings.Trim(val, `"'`)
			os.Setenv(key, val)
		}
	}
}

func main() {
	loadEnv()
	initRuntimeSettings()
	os.MkdirAll("static/generated", 0755)
	go startRateMetricsLogger(time.Duration(envInt("RATE_METRICS_LOG_INTERVAL_SECONDS", 60)) * time.Second)

	http.HandleFunc("/ws", handleWebSocket)
	// API endpoint to create a new room and return its ID
	http.HandleFunc("/api/create-room", handleCreateRoom)
	// API endpoint to list active rooms
	http.HandleFunc("/api/rooms", handleListRooms)
	// Serve room pages: /room/{id} serves the same index.html
	http.HandleFunc("/room/", handleRoomPage)
	http.Handle("/", http.FileServer(http.Dir("./static")))

	apiKey := os.Getenv("GEMINI_API_KEY")
	if apiKey == "" {
		fmt.Println("⚠️  Warning: GEMINI_API_KEY environment variable not set. Running in MOCK Vector mode.")
	} else {
		fmt.Println("🔑 Gemini API key loaded successfully.")
	}

	fmt.Println("🚀 Comic Chat Server running on http://localhost:8080")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		fmt.Printf("Error starting server: %v\n", err)
	}
}

func envInt(name string, defaultValue int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return defaultValue
	}
	val, err := strconv.Atoi(raw)
	if err != nil || val <= 0 {
		return defaultValue
	}
	return val
}

func initRuntimeSettings() {
	maxMessageChars = envInt("MAX_MESSAGE_CHARS", 500)
	maxWSPayloadBytes = envInt("MAX_WS_PAYLOAD_BYTES", 4096)
	limiter = newIPRateLimiter(
		envInt("PER_IP_BURST_COUNT", 5),
		time.Duration(envInt("PER_IP_BURST_WINDOW_SECONDS", 10))*time.Second,
		envInt("PER_IP_SUSTAINED_COUNT", 30),
		time.Duration(envInt("PER_IP_SUSTAINED_WINDOW_SECONDS", 60))*time.Second,
		time.Duration(envInt("IP_STATE_TTL_SECONDS", 600))*time.Second,
	)
}

func extractClientIP(r *http.Request) string {
	xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For"))
	if xff != "" {
		parts := strings.Split(xff, ",")
		for _, part := range parts {
			ip := strings.TrimSpace(part)
			if net.ParseIP(ip) != nil {
				return ip
			}
		}
	}
	remoteAddr := strings.TrimSpace(r.RemoteAddr)
	if net.ParseIP(remoteAddr) != nil {
		return remoteAddr
	}
	host, _, err := net.SplitHostPort(remoteAddr)
	if err == nil && host != "" {
		return host
	}
	return remoteAddr
}

func enqueueClientMessage(client *Client, payload []byte) {
	defer func() {
		_ = recover()
	}()
	select {
	case client.send <- payload:
	default:
	}
}

func startRateMetricsLogger(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for range ticker.C {
		allowed := atomic.LoadUint64(&rateAllowedTotal)
		dropped := atomic.LoadUint64(&rateDroppedTotal)
		active := limiter.activeIPs(time.Now())
		fmt.Printf("rate_metrics allowed_total=%d dropped_total=%d active_ips=%d\n", allowed, dropped, active)
	}
}

func handleCreateRoom(w http.ResponseWriter, r *http.Request) {
	roomID := fmt.Sprintf("%s-%d", randomWord(), rand.Intn(9999))
	getOrCreateRoom(roomID)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"room_id": roomID})
}

func handleListRooms(w http.ResponseWriter, r *http.Request) {
	roomsMu.Lock()
	var roomList []map[string]interface{}
	for id, room := range rooms {
		room.mu.Lock()
		count := len(room.clients)
		room.mu.Unlock()
		roomList = append(roomList, map[string]interface{}{"id": id, "users": count})
	}
	roomsMu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(roomList)
}

func handleRoomPage(w http.ResponseWriter, r *http.Request) {
	// Serve the same index.html for any /room/{id} path
	http.ServeFile(w, r, "static/index.html")
}

func randomWord() string {
	words := []string{"gotham", "comic", "panel", "hero", "villain", "bubble", "sketch", "ink", "strip", "pow", "zap", "boom", "splash", "chat"}
	return words[rand.Intn(len(words))]
}

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	// Extract room from query: /ws?room=xxx
	roomID := r.URL.Query().Get("room")
	if roomID == "" {
		roomID = "lobby"
	}
	room := getOrCreateRoom(roomID)
	clientIP := extractClientIP(r)

	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "Webserver doesn't support hijacking", http.StatusInternalServerError)
		return
	}
	conn, bufrw, err := hj.Hijack()
	if err != nil {
		return
	}

	key := r.Header.Get("Sec-WebSocket-Key")
	h := sha1.New()
	h.Write([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	accept := base64.StdEncoding.EncodeToString(h.Sum(nil))

	bufrw.WriteString("HTTP/1.1 101 Switching Protocols\r\n")
	bufrw.WriteString("Upgrade: websocket\r\n")
	bufrw.WriteString("Connection: Upgrade\r\n")
	bufrw.WriteString("Sec-WebSocket-Accept: " + accept + "\r\n\r\n")
	bufrw.Flush()

	client := &Client{
		conn: conn,
		send: make(chan []byte, 256),
	}

	room.mu.Lock()
	room.clients[client] = true
	room.mu.Unlock()

	defer func() {
		room.mu.Lock()
		delete(room.clients, client)
		room.mu.Unlock()
		conn.Close()
	}()

	go func() {
		for msg := range client.send {
			writeFrame(conn, msg)
		}
	}()

	for {
		payload, err := readFrame(conn, maxWSPayloadBytes)
		if err != nil {
			break
		}

		var req struct {
			Type   string `json:"type"`
			Sender string `json:"sender"`
			Text   string `json:"text"`
		}
		if err := json.Unmarshal([]byte(payload), &req); err != nil || req.Type != "chat_message" {
			continue
		}
		req.Sender = strings.TrimSpace(req.Sender)
		req.Text = strings.TrimSpace(req.Text)
		if req.Sender == "" || req.Text == "" || len(req.Text) > maxMessageChars {
			statusMsg, _ := json.Marshal(map[string]interface{}{
				"type":   "status",
				"status": "invalid_message",
				"sender": req.Sender,
			})
			enqueueClientMessage(client, statusMsg)
			continue
		}
		if !limiter.allow(clientIP, time.Now()) {
			atomic.AddUint64(&rateDroppedTotal, 1)
			statusMsg, _ := json.Marshal(map[string]interface{}{
				"type":   "status",
				"status": "rate_limited",
				"sender": req.Sender,
			})
			enqueueClientMessage(client, statusMsg)
			continue
		}
		atomic.AddUint64(&rateAllowedTotal, 1)

		go func(sender, text string) {
			room.mu.Lock()

			// Add to room's active speakers if not present
			found := false
			for _, s := range room.activeSpeakers {
				if s == sender {
					found = true
					break
				}
			}
			if !found {
				room.activeSpeakers = append(room.activeSpeakers, sender)
			}

			shouldSplit := false
			if room.currentPanelID == "" || len(room.currentPanelMessages) == 0 {
				shouldSplit = true
			} else {
				// Create new scene ONLY when the same character talks again in the same scene
				for _, m := range room.currentPanelMessages {
					if m.Sender == sender {
						shouldSplit = true
						break
					}
				}
			}

			if shouldSplit {
				room.currentPanelID = fmt.Sprintf("panel_%d_%d", time.Now().Unix(), rand.Intn(1000))
				room.currentPanelMessages = nil
			}
			room.currentPanelMessages = append(room.currentPanelMessages, ChatMessage{Sender: sender, Text: text})

			messagesCopy := make([]ChatMessage, len(room.currentPanelMessages))
			copy(messagesCopy, room.currentPanelMessages)

			speakersCopy := make([]string, len(room.activeSpeakers))
			copy(speakersCopy, room.activeSpeakers)

			panelID := room.currentPanelID
			room.mu.Unlock()

			// Broadcast status: generating
			statusMsg, _ := json.Marshal(map[string]interface{}{
				"type":   "status",
				"status": "generating",
				"sender": sender,
			})
			room.mu.Lock()
			for c := range room.clients {
				select {
				case c.send <- statusMsg:
				default:
					close(c.send)
					delete(room.clients, c)
				}
			}
			room.mu.Unlock()

			startTime := time.Now()
			imageData, ext, isFallback := runGeminiComicAI(messagesCopy, speakersCopy, room.Seed)
			duration := time.Since(startTime)

			imagePath := fmt.Sprintf("static/generated/%s.%s", panelID, ext)
			imageUrl := fmt.Sprintf("/generated/%s.%s", panelID, ext)

			err := os.WriteFile(imagePath, imageData, 0644)
			if err != nil {
				fmt.Printf("Error saving panel image: %v\n", err)
			}

			uniqueSenders := make(map[string]bool)
			var sendersList []string
			for _, m := range messagesCopy {
				if !uniqueSenders[m.Sender] {
					uniqueSenders[m.Sender] = true
					sendersList = append(sendersList, m.Sender)
				}
			}
			participantsStr := strings.Join(sendersList, " & ")

			res, _ := json.Marshal(map[string]interface{}{
				"type":        "new_panel",
				"panel_id":    panelID,
				"sender":      participantsStr,
				"dialogue":    text,
				"image_url":   imageUrl,
				"is_fallback": isFallback,
				"duration_ms": duration.Milliseconds(),
			})

			room.mu.Lock()
			for c := range room.clients {
				select {
				case c.send <- res:
				default:
					close(c.send)
					delete(room.clients, c)
				}
			}
			room.mu.Unlock()
		}(req.Sender, req.Text)
	}
}

// Queries Gemini to generate a high-quality comic panel image
func runGeminiComicAI(messages []ChatMessage, activeSpeakers []string, seed int) (imageData []byte, ext string, isFallback bool) {
	apiKey := os.Getenv("GEMINI_API_KEY")
	if apiKey == "" {
		return []byte(drawErrorSVG("GEMINI_API_KEY is not set")), "svg", true
	}

	var historyBuilder strings.Builder
	for _, m := range messages {
		historyBuilder.WriteString(fmt.Sprintf("%s: %s\n", m.Sender, m.Text))
	}
	historyStr := historyBuilder.String()

	var speakers []string
	var speakerProfiles []string
	for i, s := range activeSpeakers {
		lowerName := strings.ToLower(s)
		speakers = append(speakers, lowerName)

		// Map index to a specific consistent character design profile
		switch i {
		case 0:
			speakerProfiles = append(speakerProfiles, fmt.Sprintf("- Character '%s': A boy with a round face, dot eyes, cloud-like wavy curly hair on top, wearing a simple long-sleeve crew-neck shirt.", lowerName))
		case 1:
			speakerProfiles = append(speakerProfiles, fmt.Sprintf("- Character '%s': A boy wearing a simple baseball cap (with visor pointing to the side/front) and a crew-neck shirt. He has dot eyes and a simple nose.", lowerName))
		default:
			speakerProfiles = append(speakerProfiles, fmt.Sprintf("- Character '%s': A person with a round face, spiky hair, and a crew-neck shirt.", lowerName))
		}
	}
	speakersListStr := strings.Join(speakers, ", ")
	profilesStr := strings.Join(speakerProfiles, "\n")

	instruction := fmt.Sprintf(`Draw a highly detailed classic 1960s black-and-white newspaper comic strip panel (like Mafalda or Peanuts).
STYLE: Traditional ink on paper, detailed shading, expressive characters, hand-drawn comic style. No colors.
COMPOSITION: Waist-up medium shot (torso and head only). Characters stand facing each other.
CRITICAL INSTRUCTION: You MUST maintain exact visual consistency for the characters across different panels. Do NOT change their core design, clothing, or layout.
CHARACTERS: You must draw exactly these characters: %s.
Even if a character is silent, they MUST be drawn listening or reacting.
You MUST draw each character consistently according to their description:
%s
BUBBLES: Include hand-drawn speech balloons positioned close above the speaking character's head with a tiny curved pointer tail. 
The dialogue should be written inside the bubbles clearly.
DIALOGUE TO INCLUDE:
%s`, speakersListStr, profilesStr, historyStr)

	// Using the Gemini Image generation endpoint
	apiURL := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/gemini-3.1-flash-lite-image:generateContent?key=%s", apiKey)
	reqPayload := map[string]interface{}{
		"contents": []interface{}{
			map[string]interface{}{
				"parts": []interface{}{
					map[string]string{"text": instruction},
				},
			},
		},
		"generationConfig": map[string]interface{}{
			"seed": seed,
		},
	}

	jsonBytes, _ := json.Marshal(reqPayload)
	req, err := http.NewRequest("POST", apiURL, bytes.NewBuffer(jsonBytes))
	if err != nil {
		return []byte(drawErrorSVG(fmt.Sprintf("Failed to create request: %v", err))), "svg", true
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "curl/8.4.0")

	resp, err := httpClient.Do(req)
	if err != nil {
		return []byte(drawErrorSVG(fmt.Sprintf("HTTP post failed: %v", err))), "svg", true
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return []byte(drawErrorSVG(fmt.Sprintf("Gemini API returned status %d", resp.StatusCode))), "svg", true
	}

	var apiRes struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					InlineData *struct {
						MimeType string `json:"mimeType"`
						Data     string `json:"data"` // base64
					} `json:"inlineData"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&apiRes); err != nil || len(apiRes.Candidates) == 0 || len(apiRes.Candidates[0].Content.Parts) == 0 {
		return []byte(drawErrorSVG("Failed to decode image response from Gemini")), "svg", true
	}

	inlineData := apiRes.Candidates[0].Content.Parts[0].InlineData
	if inlineData == nil || inlineData.Data == "" {
		return []byte(drawErrorSVG("Model did not return image data")), "svg", true
	}

	decoded, err := base64.StdEncoding.DecodeString(inlineData.Data)
	if err != nil {
		return []byte(drawErrorSVG("Failed to decode base64 image data")), "svg", true
	}

	extension := "jpeg"
	if strings.Contains(inlineData.MimeType, "png") {
		extension = "png"
	}

	return decoded, extension, false
}

func drawErrorSVG(errMsg string) string {
	return fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" width="500" height="400" viewBox="0 0 500 400">
  <rect width="500" height="400" fill="white" stroke="black" stroke-width="1.5"/>
  <text x="250" y="190" font-family="sans-serif" font-size="16" font-weight="bold" fill="#dc2626" text-anchor="middle">API Error</text>
  <text x="250" y="220" font-family="sans-serif" font-size="12" fill="#4b5563" text-anchor="middle">%s</text>
</svg>`, errMsg)
}

func readFrame(r io.Reader, maxPayloadLen int) (string, error) {
	buf := make([]byte, 2)
	_, err := io.ReadFull(r, buf)
	if err != nil {
		return "", err
	}
	op := buf[0] & 0x0f
	masked := buf[1]&0x80 != 0
	payloadLen := int(buf[1] & 0x7f)

	if op == 8 {
		return "", io.EOF
	}

	if payloadLen == 126 {
		_, err := io.ReadFull(r, buf)
		if err != nil {
			return "", err
		}
		payloadLen = int(binary.BigEndian.Uint16(buf))
	} else if payloadLen == 127 {
		lenBuf := make([]byte, 8)
		_, err := io.ReadFull(r, lenBuf)
		if err != nil {
			return "", err
		}
		payloadLen = int(binary.BigEndian.Uint64(lenBuf))
	}
	if payloadLen < 0 || payloadLen > maxPayloadLen {
		return "", fmt.Errorf("payload too large")
	}

	var mask [4]byte
	if masked {
		_, err := io.ReadFull(r, mask[:])
		if err != nil {
			return "", err
		}
	}

	payload := make([]byte, payloadLen)
	_, err = io.ReadFull(r, payload)
	if err != nil {
		return "", err
	}

	if masked {
		for i := 0; i < payloadLen; i++ {
			payload[i] ^= mask[i%4]
		}
	}
	return string(payload), nil
}

func writeFrame(w io.Writer, payload []byte) error {
	var header []byte
	header = append(header, 0x81)
	length := len(payload)
	if length < 126 {
		header = append(header, byte(length))
	} else if length <= 65535 {
		header = append(header, 126)
		lenBuf := make([]byte, 2)
		binary.BigEndian.PutUint16(lenBuf, uint16(length))
		header = append(header, lenBuf...)
	} else {
		header = append(header, 127)
		lenBuf := make([]byte, 8)
		binary.BigEndian.PutUint64(lenBuf, uint64(length))
		header = append(header, lenBuf...)
	}
	_, err := w.Write(header)
	if err != nil {
		return err
	}
	_, err = w.Write(payload)
	return err
}
