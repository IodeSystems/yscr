package service

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/iodesystems/yscr/config"
)

// TestRealtimeProxy stands up a fake oidio realtime endpoint and drives the full
// relay: browser WS → /api/audio/realtime → upstream. Verifies the key is
// injected outbound, inbound Authorization is dropped, the model query is
// forwarded, and frames relay both ways.
func TestRealtimeProxy(t *testing.T) {
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	gotAuth := make(chan string, 1)
	gotQuery := make(chan string, 1)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth <- r.Header.Get("Authorization")
		gotQuery <- r.URL.RawQuery
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		// Echo oidio's shape: on an append, emit a delta then a completed.
		for {
			_, data, err := c.ReadMessage()
			if err != nil {
				return
			}
			if strings.Contains(string(data), "input_audio_buffer.append") {
				_ = c.WriteJSON(map[string]any{"type": "conversation.item.input_audio_transcription.delta", "delta": "hel"})
				_ = c.WriteJSON(map[string]any{"type": "conversation.item.input_audio_transcription.completed", "transcript": "hello"})
			}
		}
	}))
	defer upstream.Close()

	s := &Server{cfg: &config.Config{Audio: config.AudioConfig{BaseURL: upstream.URL, APIKey: "testkey"}}}
	front := httptest.NewServer(http.HandlerFunc(s.handleRealtime))
	defer front.Close()

	wsURL := "ws" + strings.TrimPrefix(front.URL, "http") + "/api/audio/realtime?model=stt"
	// An inbound Authorization must NOT reach upstream (the proxy sets its own).
	dialer := websocket.Dialer{HandshakeTimeout: 3 * time.Second}
	c, _, err := dialer.Dial(wsURL, http.Header{"Authorization": {"Bearer client-should-be-dropped"}})
	if err != nil {
		t.Fatalf("dial front: %v", err)
	}
	defer c.Close()

	if err := c.WriteJSON(map[string]any{"type": "input_audio_buffer.append", "audio": "AAA="}); err != nil {
		t.Fatalf("write append: %v", err)
	}

	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	var delta, completed string
	for delta == "" || completed == "" {
		var m struct {
			Type       string `json:"type"`
			Delta      string `json:"delta"`
			Transcript string `json:"transcript"`
		}
		if err := c.ReadJSON(&m); err != nil {
			t.Fatalf("read: %v (delta=%q completed=%q)", err, delta, completed)
		}
		switch m.Type {
		case "conversation.item.input_audio_transcription.delta":
			delta = m.Delta
		case "conversation.item.input_audio_transcription.completed":
			completed = m.Transcript
		}
	}
	if delta != "hel" || completed != "hello" {
		t.Fatalf("relayed delta=%q completed=%q; want hel/hello", delta, completed)
	}

	if auth := <-gotAuth; auth != "Bearer testkey" {
		t.Errorf("upstream Authorization = %q; want the injected server key (inbound must be dropped)", auth)
	}
	if q := <-gotQuery; q != "model=stt" {
		t.Errorf("upstream query = %q; want model=stt forwarded", q)
	}
}
