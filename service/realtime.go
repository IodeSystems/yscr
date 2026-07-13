package service

import (
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// Realtime STT proxy — a WebSocket relay under /api/audio/realtime → oidio's
// GET /v1/realtime (OpenAI Realtime transcription schema). Same security posture
// as the HTTP audioProxy, extended to a streaming transport:
//   - fixed upstream suffix /v1/realtime (no request-controlled path → no SSRF),
//   - the query string (e.g. ?model=stt) is forwarded verbatim,
//   - the corrallm/oidio key is added on the OUTBOUND dial only; the browser
//     holds no credential and any inbound Authorization is dropped.
//
// Why this exists: batch /v1/audio/transcriptions can't start decoding until the
// clip is fully recorded (and stream=true on it is fake — it decodes the whole
// clip, then replays tokens). The realtime WS runs oidio's *streaming* recognizer
// with server-side endpointing, so partial transcripts arrive while the user
// speaks and the final lands ~0.6s after they stop — no client silence gate.

const rtHandshakeTimeout = 15 * time.Second

var rtUpgrader = websocket.Upgrader{
	// Personal LAN app; the PWA is same-origin. corrallm/oidio front auth+origin
	// upstream, and this relay adds the key, so we accept the browser here.
	CheckOrigin:     func(*http.Request) bool { return true },
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
}

// realtimeUpstream maps the audio base URL to its ws(s):// /v1/realtime endpoint,
// forwarding the client's query (?model=…) and switching http→ws / https→wss.
func (s *Server) realtimeUpstream(rawQuery string) (string, error) {
	u, err := url.Parse(strings.TrimRight(s.cfg.Audio.BaseURL, "/") + "/v1/realtime")
	if err != nil {
		return "", err
	}
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	u.RawQuery = rawQuery
	return u.String(), nil
}

// handleRealtime dials oidio first (so an upstream failure is a clean HTTP 502
// before the client upgrade), then upgrades the browser and pumps frames both
// ways until either side closes.
func (s *Server) handleRealtime(w http.ResponseWriter, r *http.Request) {
	upstream, err := s.realtimeUpstream(r.URL.RawQuery)
	if err != nil {
		http.Error(w, "bad upstream", http.StatusInternalServerError)
		return
	}

	hdr := http.Header{}
	if s.cfg.Audio.APIKey != "" {
		hdr.Set("Authorization", "Bearer "+s.cfg.Audio.APIKey)
	}
	dialer := websocket.Dialer{HandshakeTimeout: rtHandshakeTimeout}
	up, resp, err := dialer.Dial(upstream, hdr)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		log.Printf("audio realtime: dial %s: %v (upstream status %d)", upstream, err, status)
		http.Error(w, "audio backend unavailable", http.StatusBadGateway)
		return
	}
	defer up.Close()

	down, err := rtUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return // Upgrade already wrote the error response
	}
	defer down.Close()

	relayWS(down, up)
}

// relayWS copies messages between two websocket connections until either closes.
// Each connection is written by exactly one goroutine (the one reading its peer),
// so gorilla's no-concurrent-writes rule holds; a read error on one side sends a
// close to the other, and relayWS returns on the first, unblocking the peer via
// the deferred Close in the caller.
func relayWS(a, b *websocket.Conn) {
	done := make(chan struct{}, 2)
	pump := func(dst, src *websocket.Conn) {
		defer func() { done <- struct{}{} }()
		for {
			mt, data, err := src.ReadMessage()
			if err != nil {
				_ = dst.WriteControl(websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
					time.Now().Add(time.Second))
				return
			}
			if err := dst.WriteMessage(mt, data); err != nil {
				return
			}
		}
	}
	go pump(a, b)
	go pump(b, a)
	<-done
}
