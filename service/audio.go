package service

import (
	"context"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// Audio proxy — forward-only relay under /api/audio/*, mirroring autowork3's:
// keeps the corrallm/oidio key server-side (the browser holds no credential),
// fixed upstream suffix per handler (no request-controlled path → no SSRF),
// hop-by-hop + inbound Authorization stripped, capped upload. The Bearer is
// added on the OUTBOUND request only and never echoed.
//
//	GET  /api/audio/capabilities   → GET  {base}/v1/capabilities
//	POST /api/audio/transcriptions → POST {base}/v1/audio/transcriptions (STT)
//	POST /api/audio/speech         → POST {base}/v1/audio/speech         (TTS)
//	GET  /api/audio/config         → {stt_model, tts_model, tts_voice}   (UI)

const (
	audioMaxUpload   = 25 << 20 // 25 MiB (corrallm STT cap)
	audioCapsTimeout = 15 * time.Second
	audioSTTTimeout  = 60 * time.Second
	audioTTSTimeout  = 120 * time.Second
)

var audioHopByHop = []string{
	"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"Te", "Trailer", "Transfer-Encoding", "Upgrade", "Authorization",
}

// audioClient never follows redirects off the fixed upstream (SSRF guard).
var audioClient = &http.Client{
	Timeout: audioTTSTimeout + 30*time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

func (s *Server) audioEnabled() bool { return s.cfg.Audio.BaseURL != "" }

func (s *Server) registerAudio(mux *http.ServeMux) {
	if !s.audioEnabled() {
		log.Printf("audio proxy: disabled (no audio base_url)")
		return
	}
	log.Printf("audio proxy: /api/audio/* → %s", s.cfg.Audio.BaseURL)
	mux.HandleFunc("GET /api/audio/capabilities", s.audioProxy("/v1/capabilities", audioCapsTimeout, 0))
	mux.HandleFunc("POST /api/audio/transcriptions", s.audioProxy("/v1/audio/transcriptions", audioSTTTimeout, audioMaxUpload))
	mux.HandleFunc("POST /api/audio/speech", s.audioProxy("/v1/audio/speech", audioTTSTimeout, 1<<20))
	mux.HandleFunc("GET /api/audio/config", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"stt_model": s.cfg.Audio.STTModel,
			"tts_model": s.cfg.Audio.TTSModel,
			"tts_voice": s.cfg.Audio.TTSVoice,
		})
	})
}

// audioProxy forwards to a single fixed upstream suffix with the server-side
// key. maxBody > 0 caps the request body.
func (s *Server) audioProxy(suffix string, timeout time.Duration, maxBody int64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		upstream := strings.TrimRight(s.cfg.Audio.BaseURL, "/") + suffix
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()

		var body io.Reader = r.Body
		if maxBody > 0 {
			body = http.MaxBytesReader(w, r.Body, maxBody)
		}
		req, err := http.NewRequestWithContext(ctx, r.Method, upstream, body)
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		// Copy request headers except hop-by-hop + inbound Authorization.
		for k, vs := range r.Header {
			if isHopByHop(k) {
				continue
			}
			for _, v := range vs {
				req.Header.Add(k, v)
			}
		}
		if s.cfg.Audio.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+s.cfg.Audio.APIKey)
		}

		resp, err := audioClient.Do(req)
		if err != nil {
			log.Printf("audio proxy: %s %s: %v", r.Method, suffix, err)
			http.Error(w, "audio backend unavailable", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()

		for k, vs := range resp.Header {
			if isHopByHop(k) {
				continue
			}
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}
}

func isHopByHop(h string) bool {
	for _, x := range audioHopByHop {
		if strings.EqualFold(h, x) {
			return true
		}
	}
	return false
}
