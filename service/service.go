// Package service is the yscr daemon: it wires the concierge + source plugins
// from config and serves them over HTTP, plus the embedded PWA and Web Push.
package service

import (
	"context"
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
	runner    agent.LLMRunner
	conc      *concierge.Concierge
	summ      *summarizer
	sources   []source.Source
	push      *pushHub
	sse       *sseHub
	cue       *cueRunner    // nil unless Cue.Enabled + a durable store
	cuegen    *cueGenerator // nil unless Cue.Enabled + store + goals
	sessionID string
}

// New builds the service: the concierge on the configured LLM endpoint, the
// enabled source plugins, durable state (Postgres, if configured), and push.
func New(cfg *config.Config) (*Server, error) {
	var runner agent.LLMRunner = llm.NewClient(cfg.LLM.BaseURL, cfg.LLM.APIKey, cfg.LLM.Model)

	// Durable state: concierge conversation + push subscriptions in Postgres
	// when a DSN is set; else in-memory (ephemeral).
	var convStore agent.Store = store.NewMem()
	var pg *store.PG
	if cfg.Database != "" {
		p, err := store.NewPG(context.Background(), cfg.Database)
		if err != nil {
			return nil, err
		}
		pg, convStore = p, p
	}

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

	ph, err := newPushHub(cfg, pg)
	if err != nil {
		return nil, err
	}
	s := &Server{
		cfg:       cfg,
		runner:    runner,
		conc:      concierge.New(runner, convStore, sources...),
		sources:   sources,
		push:      ph,
		sse:       newSSEHub(),
		sessionID: "primary",
	}
	s.summ = newSummarizer(runner, s.broadcastActivity, s.broadcastFleet)
	// Outbound task scheduler (nil unless Cue.Enabled + Postgres). Drives off the
	// fleet watcher; see cue.go and the cue package.
	s.cue = newCueRunner(cfg.Cue, pg, sources, func(title, body string) { s.Notify(title, body) })
	// The LLM generator that proposes tasks into the cue (nil unless enabled +
	// store + goals). Guard on pg != nil so we never pass a typed-nil enqueuer.
	if cfg.Cue.Enabled && pg != nil {
		s.cuegen = newCueGenerator(configCueGen{Goals: cfg.Cue.Goals, GenInterval: cfg.Cue.GenInterval}, runner, pg, s.fleetStates)
	}
	return s, nil
}

// broadcastActivity emits a background-activity SSE event (the concierge working
// on a session in the background — e.g. summarizing). kind is "summarizing" or
// "idle".
func (s *Server) broadcastActivity(kind, key, title string) {
	s.sse.broadcast(sseMsg{event: "activity", data: mustJSON(map[string]string{"kind": kind, "session": key, "title": title})})
}

// broadcastFleet nudges connected clients to re-pull /api/fleet.
func (s *Server) broadcastFleet() { s.sse.broadcast(sseMsg{event: "fleet", data: "{}"}) }

// Notify pushes a notification to every subscribed client. The narration layer
// (and any alerting) calls this. Returns how many were delivered.
func (s *Server) Notify(title, body string) int { return s.push.notify(title, body) }

// Handler builds the HTTP routes (API + the embedded PWA).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/converse", s.handleConverse)
	mux.HandleFunc("GET /api/fleet", s.handleFleet)
	mux.HandleFunc("POST /api/answer", s.handleAnswer)
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
	s.registerAudio(mux)
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
	states := s.fleetStates(r.Context())
	// Overlay the throttled LLM digest where we have one; the raw source tail
	// stands in until the first summary lands.
	for i := range states {
		if d := s.summ.summaryFor(sessionKey(states[i].Ref)); d != "" {
			states[i].Summary = d
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": states})
}

// handleAnswer submits a tap-to-answer directly to a source's Actor (no LLM):
// {source, id, questionnaire_id, answers:{field_key: value}}. It re-fetches the
// live questionnaire, validates against it (same path as the concierge tool),
// then Acts and nudges the fleet. The concierge conversation is the other way
// to answer; this is the visual/tap path.
func (s *Server) handleAnswer(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Source          string         `json:"source"`
		ID              string         `json:"id"`
		QuestionnaireID string         `json:"questionnaire_id"`
		Answers         map[string]any `json:"answers"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Source == "" || in.ID == "" || in.Answers == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "source, id, and answers are required"})
		return
	}
	src := s.sourceByID(in.Source)
	if src == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unknown source " + in.Source})
		return
	}
	actor, ok := src.(source.Actor)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "source cannot accept answers"})
		return
	}
	// Re-fetch the live questionnaire to validate against (it may have changed).
	st, err := src.State(r.Context(), in.ID)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	var q *source.Questionnaire
	for i := range st.Pending {
		if st.Pending[i].ID == in.QuestionnaireID {
			q = &st.Pending[i]
			break
		}
	}
	if q == nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "that question is no longer awaiting (already answered or changed)"})
		return
	}
	if err := source.Validate(*q, in.Answers); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	res, err := actor.Act(r.Context(), in.ID, source.Action{
		Name: "answer_questionnaire",
		Args: map[string]any{"questionnaire_id": in.QuestionnaireID, "answers": in.Answers},
	})
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	s.broadcastFleet()
	writeJSON(w, http.StatusOK, map[string]any{"result": res})
}

func (s *Server) sourceByID(id string) source.Source {
	for _, src := range s.sources {
		if src.ID() == id {
			return src
		}
	}
	return nil
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
