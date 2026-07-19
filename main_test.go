package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type testWebSocket struct {
	conn   net.Conn
	reader *bufio.Reader
}

func newWebSocketTestServer(t *testing.T) *httptest.Server {
	t.Helper()

	var handlers sync.WaitGroup
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlers.Add(1)
		defer handlers.Done()
		handleWebSocket(w, r)
	}))
	t.Cleanup(func() {
		server.Close()
		handlers.Wait()
	})
	return server
}

func newTestWebSocket(t *testing.T, serverURL, room, forwardedFor string) *testWebSocket {
	t.Helper()

	address := strings.TrimPrefix(serverURL, "http://")
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}

	request := fmt.Sprintf(
		"GET /ws?room=%s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGVzdC1rZXk=\r\nSec-WebSocket-Version: 13\r\n",
		room,
		address,
	)
	if forwardedFor != "" {
		request += "X-Forwarded-For: " + forwardedFor + "\r\n"
	}
	request += "\r\n"

	if _, err := io.WriteString(conn, request); err != nil {
		conn.Close()
		t.Fatal(err)
	}

	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodGet})
	if err != nil {
		conn.Close()
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusSwitchingProtocols {
		conn.Close()
		t.Fatalf("expected WebSocket upgrade, got %s", response.Status)
	}

	socket := &testWebSocket{conn: conn, reader: reader}
	t.Cleanup(socket.close)
	return socket
}

func (ws *testWebSocket) close() {
	ws.conn.Close()
}

func (ws *testWebSocket) sendJSON(t *testing.T, value any) {
	t.Helper()

	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ws.conn.Write(maskedTextFrame(payload)); err != nil {
		t.Fatal(err)
	}
}

func (ws *testWebSocket) awaitMessage(t *testing.T, matches func(map[string]any) bool) map[string]any {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		ws.conn.SetReadDeadline(deadline)
		payload, err := readFrame(ws.reader, 1<<20)
		if err != nil {
			t.Fatal(err)
		}

		var message map[string]any
		if err := json.Unmarshal([]byte(payload), &message); err != nil {
			t.Fatal(err)
		}
		if matches(message) {
			return message
		}
	}

	t.Fatal("timed out waiting for WebSocket message")
	return nil
}

func maskedTextFrame(payload []byte) []byte {
	mask := [4]byte{1, 2, 3, 4}
	frame := []byte{0x81}

	switch {
	case len(payload) < 126:
		frame = append(frame, 0x80|byte(len(payload)))
	case len(payload) <= 65535:
		frame = append(frame, 0x80|126, 0, 0)
		binary.BigEndian.PutUint16(frame[len(frame)-2:], uint16(len(payload)))
	default:
		frame = append(frame, 0x80|127)
		size := make([]byte, 8)
		binary.BigEndian.PutUint64(size, uint64(len(payload)))
		frame = append(frame, size...)
	}

	frame = append(frame, mask[:]...)
	for i, value := range payload {
		frame = append(frame, value^mask[i%len(mask)])
	}
	return frame
}

func resetWebSocketTestState(t *testing.T, shortLimit, maxPayload int) {
	t.Helper()

	t.Setenv("GEMINI_API_KEY", "")
	if err := os.MkdirAll("static/generated", 0o755); err != nil {
		t.Fatal(err)
	}

	roomsMu.Lock()
	rooms = make(map[string]*Room)
	roomsMu.Unlock()

	limiter = newIPRateLimiter(shortLimit, time.Minute, 100, time.Minute, time.Minute)
	maxMessageChars = 500
	maxWSPayloadBytes = maxPayload
	atomic.StoreUint64(&rateAllowedTotal, 0)
	atomic.StoreUint64(&rateDroppedTotal, 0)
}

func TestWebSocketAcceptsAndGroupsMessages(t *testing.T) {
	resetWebSocketTestState(t, 10, 4096)
	server := newWebSocketTestServer(t)

	socket := newTestWebSocket(t, server.URL, "grouping", "203.0.113.10")

	sendChat := func(sender, text string) map[string]any {
		socket.sendJSON(t, map[string]string{
			"type":   "chat_message",
			"sender": sender,
			"text":   text,
		})
		return socket.awaitMessage(t, func(message map[string]any) bool {
			return message["type"] == "new_panel"
		})
	}

	first := sendChat("Alex", "Hello")
	second := sendChat("Blair", "Hi")
	third := sendChat("Alex", "Again")

	if first["panel_id"] != second["panel_id"] {
		t.Fatal("different speakers should update the current panel")
	}
	if second["panel_id"] == third["panel_id"] {
		t.Fatal("a repeated speaker should start a new panel")
	}
	if second["sender"] != "Alex & Blair" {
		t.Fatalf("expected ordered character names, got %q", second["sender"])
	}
	if got := atomic.LoadUint64(&rateAllowedTotal); got != 3 {
		t.Fatalf("expected 3 allowed messages, got %d", got)
	}
}

func TestWebSocketRejectsInvalidMessage(t *testing.T) {
	resetWebSocketTestState(t, 10, 4096)
	server := newWebSocketTestServer(t)

	socket := newTestWebSocket(t, server.URL, "invalid", "203.0.113.11")
	socket.sendJSON(t, map[string]string{
		"type":   "chat_message",
		"sender": "Alex",
		"text":   strings.Repeat("x", maxMessageChars+1),
	})

	message := socket.awaitMessage(t, func(message map[string]any) bool {
		return message["status"] == "invalid_message"
	})
	if message["sender"] != "Alex" {
		t.Fatalf("expected sender Alex, got %q", message["sender"])
	}
	if got := atomic.LoadUint64(&rateAllowedTotal); got != 0 {
		t.Fatalf("invalid message counted as allowed: %d", got)
	}
}

func TestWebSocketRateLimitIsSharedByIP(t *testing.T) {
	resetWebSocketTestState(t, 1, 4096)
	server := newWebSocketTestServer(t)

	first := newTestWebSocket(t, server.URL, "rate-limit", "198.51.100.1, 203.0.113.12, 10.0.0.1")
	second := newTestWebSocket(t, server.URL, "rate-limit", "198.51.100.2, 203.0.113.12, 10.0.0.2")

	first.sendJSON(t, map[string]string{"type": "chat_message", "sender": "Alex", "text": "First"})
	first.awaitMessage(t, func(message map[string]any) bool {
		return message["status"] == "generating"
	})

	second.sendJSON(t, map[string]string{"type": "chat_message", "sender": "Blair", "text": "Second"})
	second.awaitMessage(t, func(message map[string]any) bool {
		return message["status"] == "rate_limited"
	})

	if got := atomic.LoadUint64(&rateDroppedTotal); got != 1 {
		t.Fatalf("expected 1 dropped message, got %d", got)
	}
}

func TestWebSocketClosesOnOversizedFrame(t *testing.T) {
	resetWebSocketTestState(t, 10, 32)
	server := newWebSocketTestServer(t)

	socket := newTestWebSocket(t, server.URL, "oversized", "203.0.113.13")
	if _, err := socket.conn.Write(maskedTextFrame([]byte(strings.Repeat("x", 33)))); err != nil {
		t.Fatal(err)
	}

	socket.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := socket.reader.ReadByte(); err == nil {
		t.Fatal("expected oversized frame to close the connection")
	} else if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
		t.Fatal("server left the oversized WebSocket connection open")
	}
}

func TestHTTPAndUIContracts(t *testing.T) {
	resetWebSocketTestState(t, 10, 4096)

	createRequest := httptest.NewRequest(http.MethodPost, "/api/create-room", nil)
	createResponse := httptest.NewRecorder()
	handleCreateRoom(createResponse, createRequest)
	if createResponse.Code != http.StatusOK {
		t.Fatalf("create room returned %d", createResponse.Code)
	}

	var created map[string]string
	if err := json.Unmarshal(createResponse.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created["room_id"] == "" {
		t.Fatal("create room returned an empty room ID")
	}

	page, err := os.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(page)
	for _, expected := range []string{
		"CREATE NEW ROOM",
		"COPY LINK",
		"SHARE STRIP",
		"chat messages:",
		"Too many messages. Please wait a moment.",
		"class=\"panel-sender\"",
	} {
		if !strings.Contains(html, expected) {
			t.Errorf("UI is missing %q", expected)
		}
	}
	if strings.Contains(strings.ToLower(html), "gemini generating") {
		t.Fatal("UI exposes implementation-specific generation text")
	}
}
