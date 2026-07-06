package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/iodesystems/yscr/source"
)

// watchInterval is how often the fleet is polled for material changes.
const watchInterval = 12 * time.Second

// ── SSE hub ─────────────────────────────────────────────────────────

type sseMsg struct {
	event string
	data  string
}

type sseHub struct {
	mu      sync.Mutex
	clients map[chan sseMsg]struct{}
}

func newSSEHub() *sseHub { return &sseHub{clients: map[chan sseMsg]struct{}{}} }

func (h *sseHub) subscribe() (chan sseMsg, func()) {
	ch := make(chan sseMsg, 32)
	h.mu.Lock()
	h.clients[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.clients, ch)
		h.mu.Unlock()
		close(ch)
	}
}

func (h *sseHub) broadcast(m sseMsg) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.clients {
		select {
		case ch <- m:
		default: // slow client: drop rather than block
		}
	}
}

// serveStream is the GET /api/stream SSE handler: live fleet pings + notices.
func (s *Server) serveStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ch, unsubscribe := s.sse.subscribe()
	defer unsubscribe()

	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		case m, ok := <-ch:
			if !ok {
				return
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", m.event, m.data)
			flusher.Flush()
		}
	}
}

// ── fleet aggregation ───────────────────────────────────────────────

// fleetStates aggregates List+State across every source (a down source is
// skipped). Shared by GET /api/fleet and the watcher.
func (s *Server) fleetStates(ctx context.Context) []source.State {
	states := []source.State{}
	for _, src := range s.sources {
		refs, err := src.List(ctx)
		if err != nil {
			continue
		}
		for _, ref := range refs {
			if st, err := src.State(ctx, ref.ID); err == nil {
				states = append(states, st)
			}
		}
	}
	return states
}

// ── watcher: diff fleet → SSE + web push ────────────────────────────

type snap struct {
	status  source.Status
	pending int
}

func notable(st source.Status) bool {
	return st == source.StatusAwaitingUser || st == source.StatusBlocked || st == source.StatusFailed
}

// Start launches the background fleet watcher. Idempotent-safe to call once.
func (s *Server) Start() {
	go s.watch(context.Background())
}

func (s *Server) watch(ctx context.Context) {
	prev := map[string]snap{}
	ticker := time.NewTicker(watchInterval)
	defer ticker.Stop()

	// Prime the baseline without notifying, so a restart doesn't re-announce
	// everything already in flight.
	for _, st := range s.fleetStates(ctx) {
		prev[key(st)] = snap{st.Status, len(st.Pending)}
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			states := s.fleetStates(ctx)
			s.summ.observe(ctx, states) // throttled background digests
			cur := make(map[string]snap, len(states))
			changed := false
			for _, st := range states {
				k := key(st)
				now := snap{st.Status, len(st.Pending)}
				cur[k] = now
				old, existed := prev[k]
				if !existed || old != now {
					changed = true
				}
				if title, body, ok := material(old, existed, st); ok {
					s.sse.broadcast(sseMsg{event: "notice", data: mustJSON(map[string]string{"title": title, "body": body})})
					s.Notify(title, body)
				}
			}
			if changed || len(cur) != len(prev) {
				// Prompt connected clients to re-pull the fleet.
				s.sse.broadcast(sseMsg{event: "fleet", data: "{}"})
			}
			prev = cur
		}
	}
}

// material decides whether a transition warrants a proactive notification and
// returns its (title, body). Fires on entering a notable status, or on a new
// decision (pending count rising).
func material(old snap, existed bool, st source.State) (title, body string, ok bool) {
	now := snap{st.Status, len(st.Pending)}
	name := st.Ref.Title
	if name == "" {
		name = st.Ref.ID
	}
	label := st.Ref.Source + "/" + name

	// A new decision awaiting the user (pending rose, or first-seen with any).
	if now.pending > 0 && (!existed || now.pending > old.pending) {
		return label, fmt.Sprintf("%d decision(s) awaiting you.", now.pending), true
	}
	// Entered a notable status.
	if notable(st.Status) && (!existed || old.status != st.Status) && st.Status != source.StatusAwaitingUser {
		switch st.Status {
		case source.StatusBlocked:
			return label, "is blocked.", true
		case source.StatusFailed:
			return label, "failed.", true
		}
	}
	return "", "", false
}

func key(st source.State) string { return st.Ref.Source + "/" + st.Ref.ID }

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
