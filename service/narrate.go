// Narration turns a session's live output stream into spoken, conversational
// updates — the "reduce the terminal to a conversation" layer. It consumes the
// same source.Observe stream the raw tail uses, but instead of forwarding bytes
// it buffers them and, on a cadence, asks the LLM for ONE natural spoken
// sentence about what's happening (conveying magnitude, not reciting output).
// The result is broadcast as a "narration" SSE event the PWA shows + speaks.
//
// Like the watch tail it's user-triggered per session (POST/DELETE
// /api/narrate/{source}/{id}); ambient auto-narration of active sessions is a
// later slice.
package service

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"

	"github.com/iodesystems/yscr/source"
)

const (
	narrateInterval = 8 * time.Second // at most one narration per session per tick
	narrateMaxChars = 4000            // cap the raw delta fed to the LLM
	narrateTimeout  = 30 * time.Second
)

const narrateSystem = `You are a fleet concierge narrating a work session ALOUD to someone who is not looking at the screen — they will hear this spoken. You are given recent raw terminal output, which may include shell prompts, keystroke echoes, and redraw noise.

Say what is happening right now in ONE natural spoken sentence, present tense, at most 20 words — the kind of thing you'd say aloud, not read off a screen.

Convey magnitude without reciting details: "it's churning through the test suite, dozens of files in, nothing failing yet" — never a list, never quoted output. Sound aware of the detail without reading it out; the tone should imply there's more there without stating it. If nothing meaningful has changed since your previous line, reply with exactly "-" (a single hyphen) and nothing else.`

// narrator asks the LLM for a spoken one-liner about a session's recent output.
type narrator struct {
	runner   agent.LLMRunner
	interval time.Duration // narration cadence; overridable in tests
}

func newNarrator(runner agent.LLMRunner) *narrator {
	return &narrator{runner: runner, interval: narrateInterval}
}

// line produces a spoken narration of `delta` given the prior line, or "" when
// there's nothing worth saying (the model replies "-", or on any error).
func (n *narrator) line(ctx context.Context, title, prev, delta string) string {
	delta = strings.TrimSpace(delta)
	if delta == "" || n.runner == nil {
		return ""
	}
	if len(delta) > narrateMaxChars {
		delta = delta[len(delta)-narrateMaxChars:]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Session: %s\n", title)
	if prev != "" {
		fmt.Fprintf(&b, "Your previous line: %s\n", prev)
	}
	fmt.Fprintf(&b, "Recent output:\n%s", delta)

	ctx, cancel := context.WithTimeout(ctx, narrateTimeout)
	defer cancel()
	ch, err := n.runner.ChatStream(ctx, []llm.Message{
		{Role: "system", Content: narrateSystem},
		{Role: "user", Content: b.String()},
	}, nil, nil)
	if err != nil {
		return ""
	}
	var out strings.Builder
	for chunk := range ch {
		if chunk.Error != "" {
			return ""
		}
		out.WriteString(chunk.Content)
		if chunk.Done {
			break
		}
	}
	s := strings.TrimSpace(out.String())
	if s == "-" || s == "" {
		return ""
	}
	return s
}

// narrateHub tracks active narrations (source/id → cancel), mirroring watchHub.
type narrateHub struct {
	mu     sync.Mutex
	active map[string]context.CancelFunc
}

func newNarrateHub() *narrateHub { return &narrateHub{active: map[string]context.CancelFunc{}} }

func (h *narrateHub) drop(key string) {
	h.mu.Lock()
	delete(h.active, key)
	h.mu.Unlock()
}

func (h *narrateHub) stopAll() {
	h.mu.Lock()
	for _, cancel := range h.active {
		cancel()
	}
	h.mu.Unlock()
}

// handleNarrate (POST /api/narrate/{source}/{id}) starts narrating a session's
// live output as spoken "narration" SSE events. Idempotent.
func (s *Server) handleNarrate(w http.ResponseWriter, r *http.Request) {
	srcID, id := r.PathValue("source"), r.PathValue("id")
	src := s.sourceByID(srcID)
	if src == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unknown source " + srcID})
		return
	}
	key := srcID + "/" + id

	s.narrations.mu.Lock()
	if _, ok := s.narrations.active[key]; ok {
		s.narrations.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"narrating": true})
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.narrations.active[key] = cancel
	s.narrations.mu.Unlock()

	ch, err := src.Observe(ctx, id)
	if err != nil {
		cancel()
		s.narrations.drop(key)
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	// Best-effort title for narration context.
	title := id
	if st, e := src.State(ctx, id); e == nil && st.Ref.Title != "" {
		title = st.Ref.Title
	}
	go s.narrateLoop(ctx, key, srcID, id, title, ch)
	writeJSON(w, http.StatusOK, map[string]any{"narrating": true})
}

// handleUnnarrate (DELETE /api/narrate/{source}/{id}) stops a narration.
func (s *Server) handleUnnarrate(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("source") + "/" + r.PathValue("id")
	s.narrations.mu.Lock()
	cancel, ok := s.narrations.active[key]
	s.narrations.mu.Unlock()
	if ok {
		cancel()
	}
	writeJSON(w, http.StatusOK, map[string]any{"narrating": false})
}

// narrateLoop buffers streamed lines and, each narrateInterval with pending
// output, emits one LLM narration. The LLM call runs inline so we naturally
// coalesce a busy burst into a single spoken line (the pipe file accumulates
// meanwhile — no loss). Ends on unnarrate (ctx cancel) or when the pane closes.
func (s *Server) narrateLoop(ctx context.Context, key, srcID, id, title string, ch <-chan source.Event) {
	defer s.narrations.drop(key)
	ticker := time.NewTicker(s.narr.interval)
	defer ticker.Stop()

	var buf []string
	prev := ""
	flush := func() {
		if len(buf) == 0 {
			return
		}
		delta := strings.Join(buf, "\n")
		buf = buf[:0]
		s.broadcastActivity("narrating", key, title)
		text := s.narr.line(ctx, title, prev, delta)
		s.broadcastActivity("idle", key, title)
		if text == "" {
			return
		}
		prev = text
		s.sse.broadcast(sseMsg{event: "narration", data: mustJSON(map[string]string{
			"source": srcID, "id": id, "title": title, "text": text,
		})})
	}
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				flush()
				return
			}
			buf = append(buf, ev.Content)
		case <-ticker.C:
			flush()
		}
	}
}
