package service

import (
	"context"
	"net/http"
	"sync"

	"github.com/iodesystems/yscr/source"
)

// watchHub tracks live pane tails. Each watch bridges a source.Observe stream to
// the SSE hub as "tail" events, until the client unwatches or the pane closes.
// One tail per session (source/id); starting an existing one is a no-op.
type watchHub struct {
	mu     sync.Mutex
	active map[string]context.CancelFunc // "source/id" → cancel
}

func newWatchHub() *watchHub { return &watchHub{active: map[string]context.CancelFunc{}} }

// handleWatch (POST /api/watch/{source}/{id}) starts streaming a session's live
// output to every SSE client as "tail" events {source, id, line}. Idempotent.
func (s *Server) handleWatch(w http.ResponseWriter, r *http.Request) {
	srcID, id := r.PathValue("source"), r.PathValue("id")
	src := s.sourceByID(srcID)
	if src == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unknown source " + srcID})
		return
	}
	key := srcID + "/" + id

	s.tails.mu.Lock()
	if _, ok := s.tails.active[key]; ok {
		s.tails.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"watching": true}) // already tailing
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.tails.active[key] = cancel
	s.tails.mu.Unlock()

	ch, err := src.Observe(ctx, id)
	if err != nil {
		cancel()
		s.tails.drop(key)
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	go s.pump(key, srcID, id, ch)
	writeJSON(w, http.StatusOK, map[string]any{"watching": true})
}

// handleUnwatch (DELETE /api/watch/{source}/{id}) stops a tail. Cancelling the
// context ends the Observe stream; the pump goroutine cleans up + emits tail-end.
func (s *Server) handleUnwatch(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("source") + "/" + r.PathValue("id")
	s.tails.mu.Lock()
	cancel, ok := s.tails.active[key]
	s.tails.mu.Unlock()
	if ok {
		cancel()
	}
	writeJSON(w, http.StatusOK, map[string]any{"watching": false})
}

// pump forwards Observe events to the SSE hub until the stream ends, then emits a
// tail-end marker and drops the watch. The stream ends on unwatch (ctx cancel) or
// when the pane closes.
func (s *Server) pump(key, srcID, id string, ch <-chan source.Event) {
	defer s.tails.drop(key)
	for ev := range ch {
		s.sse.broadcast(sseMsg{event: "tail", data: mustJSON(map[string]string{
			"source": srcID, "id": id, "line": ev.Content,
		})})
	}
	s.sse.broadcast(sseMsg{event: "tail-end", data: mustJSON(map[string]string{"source": srcID, "id": id})})
}

func (h *watchHub) drop(key string) {
	h.mu.Lock()
	delete(h.active, key)
	h.mu.Unlock()
}

// stopAll cancels every live tail (server shutdown).
func (h *watchHub) stopAll() {
	h.mu.Lock()
	for _, cancel := range h.active {
		cancel()
	}
	h.mu.Unlock()
}
