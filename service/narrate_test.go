package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/iodesystems/yscr/source"
)

func newNarrateServer(src *streamSource, runnerText string) *Server {
	n := newNarrator(fixedRunner{text: runnerText})
	n.interval = 10 * time.Millisecond // fast ticker for tests
	return &Server{
		sources:    []source.Source{src},
		sse:        newSSEHub(),
		narr:       n,
		narrations: newNarrateHub(),
	}
}

// narrator.line returns the LLM text, or "" when the model declines with "-".
func TestNarrator_Line(t *testing.T) {
	n := newNarrator(fixedRunner{text: "it's churning through the tests, nothing failing yet"})
	got := n.line(context.Background(), "build", "", "PASS ok pkg/a\nPASS ok pkg/b")
	if got != "it's churning through the tests, nothing failing yet" {
		t.Errorf("line = %q", got)
	}
	// "-" (nothing meaningful) → empty.
	skip := newNarrator(fixedRunner{text: "-"})
	if skip.line(context.Background(), "build", "prev", "noise") != "" {
		t.Error(`a "-" reply should produce no narration`)
	}
	// empty delta → no LLM call, empty.
	if n.line(context.Background(), "build", "", "   ") != "" {
		t.Error("empty delta should produce no narration")
	}
}

// A narration bridges buffered stream lines → one LLM "narration" SSE event.
func TestNarrate_EmitsNarrationEvent(t *testing.T) {
	src := &streamSource{id: "terminal", lines: []string{"go build", "ok"}}
	s := newNarrateServer(src, "the build just went green")

	sub, unsub := s.sse.subscribe()
	defer unsub()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/narrate/terminal/pane2", nil)
	req.SetPathValue("source", "terminal")
	req.SetPathValue("id", "%2")
	s.handleNarrate(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("narrate = %d", rec.Code)
	}

	deadline := time.After(2 * time.Second)
	for {
		select {
		case m := <-sub:
			if m.event == "narration" {
				var d map[string]string
				_ = json.Unmarshal([]byte(m.data), &d)
				if d["text"] != "the build just went green" || d["source"] != "terminal" || d["id"] != "%2" {
					t.Errorf("narration event = %v", d)
				}
				return
			}
		case <-deadline:
			t.Fatal("no narration event emitted")
		}
	}
}

func TestNarrate_UnknownSource(t *testing.T) {
	s := newNarrateServer(&streamSource{id: "terminal"}, "x")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/narrate/nope/x", nil)
	req.SetPathValue("source", "nope")
	req.SetPathValue("id", "x")
	s.handleNarrate(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("unknown source = %d; want 400", rec.Code)
	}
}

// Unnarrate cancels; a second narrate of the same session is idempotent.
func TestNarrate_UnnarrateAndIdempotent(t *testing.T) {
	src := &streamSource{id: "terminal", block: true, observed: make(chan struct{})}
	s := newNarrateServer(src, "-") // "-" so no events, just lifecycle

	narrate := func() int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/api/narrate/terminal/pane2", nil)
		req.SetPathValue("source", "terminal")
		req.SetPathValue("id", "%2")
		s.handleNarrate(rec, req)
		return rec.Code
	}
	if narrate() != http.StatusOK {
		t.Fatal("first narrate failed")
	}
	<-src.observed
	s.narrations.mu.Lock()
	n := len(s.narrations.active)
	s.narrations.mu.Unlock()
	if n != 1 {
		t.Fatalf("active narrations = %d; want 1", n)
	}
	if narrate() != http.StatusOK {
		t.Fatal("idempotent narrate failed")
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("DELETE", "/api/narrate/terminal/pane2", nil)
	req.SetPathValue("source", "terminal")
	req.SetPathValue("id", "%2")
	s.handleUnnarrate(rec, req)

	deadline := time.After(2 * time.Second)
	for {
		s.narrations.mu.Lock()
		n := len(s.narrations.active)
		s.narrations.mu.Unlock()
		if n == 0 {
			return
		}
		select {
		case <-deadline:
			t.Fatal("narration not dropped after unnarrate")
		case <-time.After(10 * time.Millisecond):
		}
	}
}
