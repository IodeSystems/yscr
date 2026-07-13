package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"sort"
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
	// Transcriptions: tee to disk when debug_save is on (diagnose cutoff/clipping).
	if s.cfg.Audio.DebugSave {
		log.Printf("audio proxy: debug_save ON → snippets in %s", s.audioDebugDir())
		mux.HandleFunc("POST /api/audio/transcriptions", s.audioTranscribeTee("/v1/audio/transcriptions", audioSTTTimeout, audioMaxUpload))
		mux.HandleFunc("GET /api/audio/debug", s.audioDebugList)
		mux.HandleFunc("GET /api/audio/debug/{file}", s.audioDebugGet)
	} else {
		mux.HandleFunc("POST /api/audio/transcriptions", s.audioProxy("/v1/audio/transcriptions", audioSTTTimeout, audioMaxUpload))
	}
	mux.HandleFunc("POST /api/audio/speech", s.audioProxy("/v1/audio/speech", audioTTSTimeout, 1<<20))
	// Streaming STT over WebSocket → oidio /v1/realtime (partials + server-side
	// endpointing; no client silence gate). See realtime.go.
	mux.HandleFunc("GET /api/audio/realtime", s.handleRealtime)
	mux.HandleFunc("GET /api/audio/config", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"stt_model":      s.cfg.Audio.STTModel,
			"realtime_model": s.cfg.Audio.RTModel,
			"tts_model":      s.cfg.Audio.TTSModel,
			"tts_voice":      s.cfg.Audio.TTSVoice,
			"debug_save":     s.cfg.Audio.DebugSave,
		})
	})
}

// audioProxy forwards to a single fixed upstream suffix with the server-side
// key. maxBody > 0 caps the request body.
func (s *Server) audioProxy(suffix string, timeout time.Duration, maxBody int64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body io.Reader = r.Body
		if maxBody > 0 {
			body = http.MaxBytesReader(w, r.Body, maxBody)
		}
		s.forwardAudio(w, r, suffix, timeout, body)
	}
}

// forwardAudio relays body to the fixed upstream suffix with the server key,
// stripping hop-by-hop + inbound Authorization, and streams the response back.
func (s *Server) forwardAudio(w http.ResponseWriter, r *http.Request, suffix string, timeout time.Duration, body io.Reader) {
	upstream := strings.TrimRight(s.cfg.Audio.BaseURL, "/") + suffix
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

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

// ── debug: save transcription snippets to disk ──────────────────────
// Off unless Audio.DebugSave. Buffers the upload, extracts + saves the audio
// file part, then forwards the ORIGINAL bytes upstream unchanged.

const audioDebugKeep = 300 // cap the debug dir to the newest N snippets

func (s *Server) audioDebugDir() string {
	if s.cfg.Audio.DebugDir != "" {
		return s.cfg.Audio.DebugDir
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".yscr", "debug-audio")
}

func (s *Server) audioTranscribeTee(suffix string, timeout time.Duration, maxBody int64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		buf, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
		if err != nil {
			http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
			return
		}
		// Best-effort save; never fail the transcription on a debug I/O error.
		if path, err := s.saveDebugAudio(r.Header.Get("Content-Type"), buf); err != nil {
			log.Printf("audio debug: save failed: %v", err)
		} else {
			log.Printf("audio debug: saved %s (%d bytes)", filepath.Base(path), len(buf))
		}
		s.forwardAudio(w, r, suffix, timeout, bytes.NewReader(buf))
	}
}

// saveDebugAudio parses the multipart upload, writes the "file" part's audio to
// the debug dir (timestamped), and prunes to the newest audioDebugKeep. Returns
// the saved path.
func (s *Server) saveDebugAudio(contentType string, buf []byte) (string, error) {
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return "", fmt.Errorf("parse content-type: %w", err)
	}
	boundary := params["boundary"]
	if boundary == "" {
		return "", fmt.Errorf("not multipart (no boundary)")
	}
	mr := multipart.NewReader(bytes.NewReader(buf), boundary)
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			return "", fmt.Errorf("no file part in upload")
		}
		if err != nil {
			return "", err
		}
		if part.FormName() != "file" {
			_ = part.Close()
			continue
		}
		ext := filepath.Ext(part.FileName())
		if ext == "" {
			ext = ".webm"
		}
		dir := s.audioDebugDir()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
		name := time.Now().Format("20060102-150405.000") + ext
		path := filepath.Join(dir, name)
		f, err := os.Create(path)
		if err != nil {
			return "", err
		}
		_, cErr := io.Copy(f, part)
		_ = part.Close()
		if closeErr := f.Close(); cErr == nil {
			cErr = closeErr
		}
		if cErr != nil {
			return "", cErr
		}
		s.pruneDebugAudio(dir)
		return path, nil
	}
}

// pruneDebugAudio keeps only the newest audioDebugKeep files (best effort).
func (s *Server) pruneDebugAudio(dir string) {
	ents, err := os.ReadDir(dir)
	if err != nil || len(ents) <= audioDebugKeep {
		return
	}
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names) // timestamped names sort chronologically
	for _, n := range names[:len(names)-audioDebugKeep] {
		_ = os.Remove(filepath.Join(dir, n))
	}
}

// audioDebugList returns the saved snippets, newest first.
func (s *Server) audioDebugList(w http.ResponseWriter, _ *http.Request) {
	ents, _ := os.ReadDir(s.audioDebugDir())
	type item struct {
		Name     string `json:"name"`
		Size     int64  `json:"size"`
		Modified string `json:"modified"`
	}
	items := make([]item, 0, len(ents))
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		items = append(items, item{Name: e.Name(), Size: fi.Size(), Modified: fi.ModTime().UTC().Format(time.RFC3339)})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name > items[j].Name }) // newest first
	writeJSON(w, http.StatusOK, map[string]any{"snippets": items})
}

// audioDebugGet serves one saved snippet. The filename is validated to a bare
// base name (no path traversal).
func (s *Server) audioDebugGet(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	if name == "" || name != filepath.Base(name) || strings.Contains(name, "..") {
		http.Error(w, "bad name", http.StatusBadRequest)
		return
	}
	http.ServeFile(w, r, filepath.Join(s.audioDebugDir(), name))
}

func isHopByHop(h string) bool {
	for _, x := range audioHopByHop {
		if strings.EqualFold(h, x) {
			return true
		}
	}
	return false
}
