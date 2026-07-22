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

// streamSource is a fake source whose Observe streams canned lines, then closes
// (or blocks until ctx cancel, when `block` is set).
type streamSource struct {
	id       string
	lines    []string
	block    bool
	observed chan struct{} // closed when Observe is called
}

func (s *streamSource) ID() string                                        { return s.id }
func (s *streamSource) List(context.Context) ([]source.SessionRef, error) { return nil, nil }
func (s *streamSource) State(context.Context, string) (source.State, error) {
	return source.State{}, nil
}
func (s *streamSource) Post(context.Context, string, string) error { return nil }
func (s *streamSource) Observe(ctx context.Context, id string) (<-chan source.Event, error) {
	if s.observed != nil {
		close(s.observed)
	}
	ch := make(chan source.Event)
	go func() {
		defer close(ch)
		for _, ln := range s.lines {
			select {
			case ch <- source.Event{Ref: source.SessionRef{Source: s.id, ID: id}, Content: ln}:
			case <-ctx.Done():
				return
			}
		}
		if s.block {
			<-ctx.Done() // hold the stream open until unwatch
		}
	}()
	return ch, nil
}

func newTestServer(src source.Source) *Server {
	return &Server{sources: []source.Source{src}, sse: newSSEHub(), tails: newWatchHub()}
}

// A watch bridges Observe lines to the SSE hub as "tail" events, then "tail-end".
func TestWatch_StreamsToSSE(t *testing.T) {
	src := &streamSource{id: "terminal", lines: []string{"line A", "line B"}}
	s := newTestServer(src)

	sub, unsub := s.sse.subscribe()
	defer unsub()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/watch/terminal/pane2", nil)
	req.SetPathValue("source", "terminal")
	req.SetPathValue("id", "%2")
	s.handleWatch(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("watch = %d", rec.Code)
	}

	var got []string
	deadline := time.After(2 * time.Second)
	for {
		select {
		case m := <-sub:
			if m.event == "tail" {
				var d map[string]string
				_ = json.Unmarshal([]byte(m.data), &d)
				got = append(got, d["line"])
			}
			if m.event == "tail-end" {
				if len(got) != 2 || got[0] != "line A" || got[1] != "line B" {
					t.Errorf("tail lines = %v", got)
				}
				return
			}
		case <-deadline:
			t.Fatalf("timed out; got %v", got)
		}
	}
}

func TestWatch_UnknownSource(t *testing.T) {
	s := newTestServer(&streamSource{id: "terminal"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/watch/nope/x", nil)
	req.SetPathValue("source", "nope")
	req.SetPathValue("id", "x")
	s.handleWatch(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("unknown source = %d; want 400", rec.Code)
	}
}

// Unwatch cancels the stream; a second watch of the same session is idempotent.
func TestWatch_UnwatchAndIdempotent(t *testing.T) {
	src := &streamSource{id: "terminal", block: true, observed: make(chan struct{})}
	s := newTestServer(src)

	watch := func() int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/api/watch/terminal/pane2", nil)
		req.SetPathValue("source", "terminal")
		req.SetPathValue("id", "%2")
		s.handleWatch(rec, req)
		return rec.Code
	}
	if watch() != http.StatusOK {
		t.Fatal("first watch failed")
	}
	<-src.observed // Observe started
	s.tails.mu.Lock()
	n := len(s.tails.active)
	s.tails.mu.Unlock()
	if n != 1 {
		t.Fatalf("active watches = %d; want 1", n)
	}
	// Second watch is a no-op (still one active).
	if watch() != http.StatusOK {
		t.Fatal("idempotent watch failed")
	}

	// Unwatch cancels; the pump goroutine drops the entry.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("DELETE", "/api/watch/terminal/pane2", nil)
	req.SetPathValue("source", "terminal")
	req.SetPathValue("id", "%2")
	s.handleUnwatch(rec, req)

	deadline := time.After(2 * time.Second)
	for {
		s.tails.mu.Lock()
		n := len(s.tails.active)
		s.tails.mu.Unlock()
		if n == 0 {
			return
		}
		select {
		case <-deadline:
			t.Fatal("watch not dropped after unwatch")
		case <-time.After(10 * time.Millisecond):
		}
	}
}
