// Package service is the yscr daemon: it wires the concierge + source plugins
// from config and serves them over HTTP, plus the embedded PWA and Web Push.
package service

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	webpush "github.com/SherClockHolmes/webpush-go"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"

	"github.com/iodesystems/yscr/concierge"
	"github.com/iodesystems/yscr/config"
	"github.com/iodesystems/yscr/plugins/autowork"
	"github.com/iodesystems/yscr/plugins/openai"
	"github.com/iodesystems/yscr/plugins/pane"
	"github.com/iodesystems/yscr/plugins/pane/claude"
	"github.com/iodesystems/yscr/plugins/pane/terminal"
	"github.com/iodesystems/yscr/questions"
	"github.com/iodesystems/yscr/scratchpad"
	"github.com/iodesystems/yscr/source"
	"github.com/iodesystems/yscr/store"
	"github.com/iodesystems/yscr/web"
)

// Server is the running yscr service.
type Server struct {
	cfg        *config.Config
	runner     agent.LLMRunner
	conc       *concierge.Concierge
	summ       *summarizer
	sources    []source.Source
	push       *pushHub
	sse        *sseHub
	tails      *watchHub
	narr       *narrator
	narrations *narrateHub
	ambient    *ambientHub // nil unless Ambient.Enabled
	cue        *cueRunner    // nil unless Cue.Enabled + a durable store
	cuegen     *cueGenerator // nil unless Cue.Enabled + store + goals
	sched      *scheduler       // nil unless there's a durable store (scratchpad tick)
	pad        scratchpad.Store // work list behind /api/tasks + the concierge task tools; nil without Postgres
	sessionID  string
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
	var openaiSrc *openai.Plugin
	if cfg.Autowork.Enabled {
		sources = append(sources, autowork.New(cfg.Autowork.BaseURL, cfg.Autowork.Token, nil))
	}
	if cfg.OpenAISessions {
		// Durable when Postgres is available: the conversation store (entries
		// table) already persists every session's log; NewWithStore rebuilds
		// the in-memory registry from it on start so sessions survive a restart.
		var os agent.Store = store.NewMem()
		if pg != nil {
			os = pg
		}
		openaiSrc = openai.New(runner, os, "")
		sources = append(sources, openaiSrc)
	}
	if cfg.ClaudeCode.Enabled {
		adapters := []pane.Adapter{claude.New(claude.Config{Command: cfg.ClaudeCode.Command})}
		if cfg.ClaudeCode.TerminalPanes {
			adapters = append(adapters, terminal.New(terminal.Config{}))
		}
		sources = append(sources, pane.NewSet(pane.Config{}, adapters...)...)
	}

	ph, err := newPushHub(cfg, pg)
	if err != nil {
		return nil, err
	}
	s := &Server{
		cfg:        cfg,
		runner:     runner,
		conc:       concierge.New(runner, convStore, sources...),
		sources:    sources,
		push:       ph,
		sse:        newSSEHub(),
		tails:      newWatchHub(),
		narr:       newNarrator(runner),
		narrations: newNarrateHub(),
		sessionID:  "primary",
	}
	if cfg.Ambient.Enabled {
		s.ambient = newAmbientHub(cfg.Ambient)
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
	// Scratchpad scheduler tick (re-arm cron tasks, promote due one-shots into
	// the cue). Needs the durable store for both sides.
	s.sched = newScheduler(pg, pg, func(title, body string) {
		s.broadcastFleet()
		s.Notify(title, body)
	})
	// Attach the scratchpad + open-questions tools (no-op when no durable store).
	if pg != nil {
		s.pad = pg
		s.conc.WithTasks(pg)
		s.conc.WithQuestions(&pgQuestions{pg})
		s.conc.SetDecisionLog(s.logAnswers)
		s.conc.WithDecisions(&pgDecisions{pg})
	}
	// Goal plans: plan_goal batch-enqueues a decomposed goal into the cue.
	if pg != nil {
		s.conc.WithPlanGoal(pg)
	}
	// Diagrams & reports: deterministic renderers over the validated state.
	s.conc.WithReports(concierge.ReportState{
		CueTasks: s.cueTaskGraph,
		WorkList: s.workListReport,
		Fleet:    s.fleetReport,
	})

	// Run & watch: the terminal pane source spawns shell windows; foreground
	// waits poll State until the shell is idle-at-prompt again.
	for _, src := range sources {
		if src.ID() == "terminal" {
			s.conc.WithRun(src, s.waitShellIdle)
			break
		}
	}
	// Durable openai registry: rebuild in-memory session metas from the
	// persisted conversation logs so a restart re-lists prior sessions.
	if openaiSrc != nil && pg != nil {
		openaiSrc.RestoreFromStore(context.Background(), pg)
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
	mux.HandleFunc("GET /api/tasks", s.handleTasks)
	mux.HandleFunc("POST /api/tasks/{id}/done", s.handleTaskDone)
	mux.HandleFunc("GET /api/questions", s.handleQuestions)
	mux.HandleFunc("POST /api/questions/{id}/answer", s.handleQuestionAnswer)
	mux.HandleFunc("GET /api/decisions", s.handleDecisions)
	mux.HandleFunc("GET /api/reports", s.handleReports)
	mux.HandleFunc("GET /api/reports/{name}", s.handleReport)
	mux.HandleFunc("POST /api/answer", s.handleAnswer)
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	mux.HandleFunc("GET /api/push/vapid", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"public_key": s.cfg.VAPID.Public})
	})
	mux.HandleFunc("GET /api/stream", s.serveStream)
	mux.HandleFunc("POST /api/watch/{source}/{id}", s.handleWatch)
	mux.HandleFunc("DELETE /api/watch/{source}/{id}", s.handleUnwatch)
	mux.HandleFunc("POST /api/narrate/{source}/{id}", s.handleNarrate)
	mux.HandleFunc("DELETE /api/narrate/{source}/{id}", s.handleUnnarrate)
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
		Medium  string `json:"medium"` // "" | "text" | "speech" — how this turn is heard
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Message == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "message is required"})
		return
	}
	reply, err := s.conc.ConverseOn(r.Context(), s.sessionID, in.Message, in.Medium)
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
// handleTasks lists the work list (scratchpad): open tasks first, then recent
// closed ones. ?kind=todo|cue|command filters. The PWA renders it as a section;
// completing a todo is a tap away (POST /api/tasks/{id}/done).
func (s *Server) handleTasks(w http.ResponseWriter, r *http.Request) {
	if s.pad == nil {
		writeJSON(w, http.StatusOK, map[string]any{"tasks": []any{}})
		return
	}
	var kinds []scratchpad.TaskKind
	if k := r.URL.Query().Get("kind"); k != "" {
		kinds = append(kinds, scratchpad.TaskKind(k))
	}
	tasks, err := s.pad.List(r.Context(), kinds...)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": tasks})
}

// handleTaskDone marks one task done (todo tap-to-complete) or failed.
func (s *Server) handleTaskDone(w http.ResponseWriter, r *http.Request) {
	if s.pad == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "no durable store"})
		return
	}
	id := r.PathValue("id")
	done := r.URL.Query().Get("status") != "failed"
	ok, err := s.pad.Complete(r.Context(), id, done)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if !ok {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "task not open"})
		return
	}
	s.broadcastFleet() // the PWA reloads tasks with the fleet
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

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
	s.logAnswers(q, in.Answers, in.Source+"·"+in.ID+" (tap-to-answer)")
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

// pgQuestions adapts *store.PG's question methods to questions.QuestionsStore
// (the interface can't be satisfied directly: PG also has scratchpad's Add).
type pgQuestions struct{ pg *store.PG }

func (a *pgQuestions) Add(ctx context.Context, q questions.Question) (*questions.Question, error) {
	return a.pg.AddQuestion(ctx, q)
}
func (a *pgQuestions) List(ctx context.Context) ([]questions.Question, error) {
	return a.pg.ListQuestions(ctx)
}
func (a *pgQuestions) Answer(ctx context.Context, id, answer string) (bool, error) {
	return a.pg.AnswerQuestion(ctx, id, answer)
}

// handleDecisions lists the decision log newest-first (the PWA's "what have I
// decided" view). Open decisions first within their status.
func (s *Server) handleDecisions(w http.ResponseWriter, r *http.Request) {
	if s.pad == nil {
		writeJSON(w, http.StatusOK, map[string]any{"decisions": []any{}})
		return
	}
	st := &pgDecisions{pg: s.pad.(*store.PG)}
	ds, err := st.List()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	out := make([]map[string]any, 0, len(ds))
	for _, d := range ds {
		out = append(out, map[string]any{
			"id": d.ID, "question": d.Question, "field": d.Field, "answer": d.Answer,
			"context": d.Context, "status": string(d.Status), "created_at": d.CreatedAt.Unix(),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"decisions": out})
}

// handleQuestions lists the open-questions queue (open first, oldest — they've
// waited longest). The PWA renders it next to "Needs you".
func (s *Server) handleQuestions(w http.ResponseWriter, r *http.Request) {
	if s.pad == nil {
		writeJSON(w, http.StatusOK, map[string]any{"questions": []any{}})
		return
	}
	qs, err := s.pad.(*store.PG).ListQuestions(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"questions": qs})
}

// handleQuestionAnswer records a tap-to-answer (no LLM): POST with {"answer"}.
func (s *Server) handleQuestionAnswer(w http.ResponseWriter, r *http.Request) {
	if s.pad == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "no durable store"})
		return
	}
	var in struct{ Answer string `json:"answer"` }
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || strings.TrimSpace(in.Answer) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "answer is required"})
		return
	}
	ok, err := s.pad.(*store.PG).AnswerQuestion(r.Context(), r.PathValue("id"), strings.TrimSpace(in.Answer))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if !ok {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "no open question with that id"})
		return
	}
	s.broadcastFleet() // the PWA reloads questions with the fleet
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
