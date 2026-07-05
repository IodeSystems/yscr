// Package service is the yscr daemon: it wires the concierge + source plugins
// from config and serves them over HTTP, plus the embedded PWA and Web Push.
package service

import (
	"encoding/json"
	"net/http"

	webpush "github.com/SherClockHolmes/webpush-go"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"

	"github.com/iodesystems/yscr/concierge"
	"github.com/iodesystems/yscr/config"
	"github.com/iodesystems/yscr/plugins/autowork"
	"github.com/iodesystems/yscr/plugins/claudecode"
	"github.com/iodesystems/yscr/plugins/openai"
	"github.com/iodesystems/yscr/source"
	"github.com/iodesystems/yscr/store"
	"github.com/iodesystems/yscr/web"
)

// Server is the running yscr service.
type Server struct {
	cfg       *config.Config
	conc      *concierge.Concierge
	sources   []source.Source
	push      *pushHub
	sse       *sseHub
	sessionID string
}

// New builds the service: the concierge on the configured LLM endpoint, the
// enabled source plugins, and the push hub.
func New(cfg *config.Config) (*Server, error) {
	var runner agent.LLMRunner = llm.NewClient(cfg.LLM.BaseURL, cfg.LLM.APIKey, cfg.LLM.Model)

	var sources []source.Source
	if cfg.Autowork.Enabled {
		sources = append(sources, autowork.New(cfg.Autowork.BaseURL, cfg.Autowork.Token, nil))
	}
	if cfg.OpenAISessions {
		sources = append(sources, openai.New(runner, store.NewMem(), ""))
	}
	if cfg.ClaudeCode.Enabled {
		sources = append(sources, claudecode.New(claudecode.Config{Command: cfg.ClaudeCode.Command}))
	}

	ph, err := newPushHub(cfg)
	if err != nil {
		return nil, err
	}
	return &Server{
		cfg:       cfg,
		conc:      concierge.New(runner, store.NewMem(), sources...),
		sources:   sources,
		push:      ph,
		sse:       newSSEHub(),
		sessionID: "primary",
	}, nil
}

// Notify pushes a notification to every subscribed client. The narration layer
// (and any alerting) calls this. Returns how many were delivered.
func (s *Server) Notify(title, body string) int { return s.push.notify(title, body) }

// Handler builds the HTTP routes (API + the embedded PWA).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/converse", s.handleConverse)
	mux.HandleFunc("GET /api/fleet", s.handleFleet)
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	mux.HandleFunc("GET /api/push/vapid", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"public_key": s.cfg.VAPID.Public})
	})
	mux.HandleFunc("GET /api/stream", s.serveStream)
	mux.HandleFunc("POST /api/push/subscribe", s.handleSubscribe)
	mux.HandleFunc("POST /api/push/test", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"sent": s.Notify("YSCR", "Test notification — you're subscribed.")})
	})
	mux.Handle("/", http.FileServerFS(web.FS))
	return mux
}

func (s *Server) handleConverse(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Message string `json:"message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Message == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "message is required"})
		return
	}
	reply, err := s.conc.Converse(r.Context(), s.sessionID, in.Message)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"reply": reply})
}

// handleFleet aggregates List+State across every source — the non-LLM status
// channel the PWA polls (and the SSE watcher diffs).
func (s *Server) handleFleet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"sessions": s.fleetStates(r.Context())})
}

func (s *Server) handleSubscribe(w http.ResponseWriter, r *http.Request) {
	var sub webpush.Subscription
	if err := json.NewDecoder(r.Body).Decode(&sub); err != nil || sub.Endpoint == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid subscription"})
		return
	}
	s.push.add(&sub)
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
