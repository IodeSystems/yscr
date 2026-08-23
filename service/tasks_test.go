package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/iodesystems/yscr/config"
	"github.com/iodesystems/yscr/scratchpad"
)

// tasksHandler builds a bare Server with just the work-list pieces wired.
func tasksHandler(t *testing.T, pad scratchpad.Store) http.Handler {
	t.Helper()
	s := &Server{cfg: &config.Config{}, sse: newSSEHub()}
	s.pad = pad
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/tasks", s.handleTasks)
	mux.HandleFunc("POST /api/tasks/{id}/done", func(w http.ResponseWriter, r *http.Request) { s.handleTaskDone(w, r) })
	return mux
}

func TestHandleTasksEmptyWithoutStore(t *testing.T) {
	srv := httptest.NewServer(tasksHandler(t, nil))
	defer srv.Close()
	r, err := http.Get(srv.URL + "/api/tasks")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatalf("status %d", r.StatusCode)
	}
	var body struct{ Tasks []any }
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Tasks) != 0 {
		t.Fatalf("expected empty, got %v", body.Tasks)
	}
}

func getJSON(t *testing.T, url string, v any) {
	t.Helper()
	r, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatalf("GET %s: status %d", url, r.StatusCode)
	}
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		t.Fatal(err)
	}
}

func TestHandleTasksListAndDone(t *testing.T) {
	pad := scratchpad.NewMem()
	ctx := context.Background()
	added, err := pad.Add(ctx, scratchpad.Task{Prompt: "water the plants", Kind: scratchpad.KindTodo})
	if err != nil || added == nil {
		t.Fatalf("add: %v", err)
	}

	srv := httptest.NewServer(tasksHandler(t, pad))
	defer srv.Close()

	var body struct{ Tasks []scratchpad.Task }
	getJSON(t, srv.URL+"/api/tasks", &body)
	if len(body.Tasks) != 1 || body.Tasks[0].ID != added.ID {
		t.Fatalf("got %v", body.Tasks)
	}

	// Tap-to-complete.
	req, err := http.NewRequest("POST", srv.URL+"/api/tasks/"+added.ID+"/done", nil)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Body.Close()
	if r2.StatusCode != 200 {
		t.Fatalf("done status %d", r2.StatusCode)
	}

	// A second complete is a conflict (status-guarded).
	req, err = http.NewRequest("POST", srv.URL+"/api/tasks/"+added.ID+"/done", nil)
	if err != nil {
		t.Fatal(err)
	}
	r3, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r3.Body.Close()
	if r3.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409, got %d", r3.StatusCode)
	}

	// The completed task still lists (closed rows are visible).
	var body2 struct{ Tasks []scratchpad.Task }
	getJSON(t, srv.URL+"/api/tasks", &body2)
	if len(body2.Tasks) != 1 || body2.Tasks[0].Status != scratchpad.StatusCompleted {
		t.Fatalf("got %v", body2.Tasks)
	}
}

func TestHandleTasksKindFilter(t *testing.T) {
	pad := scratchpad.NewMem()
	ctx := context.Background()
	if _, err := pad.Add(ctx, scratchpad.Task{Prompt: "a todo", Kind: scratchpad.KindTodo}); err != nil {
		t.Fatal(err)
	}
	if _, err := pad.Add(ctx, scratchpad.Task{Prompt: "a cue", Kind: scratchpad.KindCue}); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(tasksHandler(t, pad))
	defer srv.Close()
	var body struct{ Tasks []scratchpad.Task }
	getJSON(t, srv.URL+"/api/tasks?kind=todo", &body)
	if len(body.Tasks) != 1 || body.Tasks[0].Kind != scratchpad.KindTodo {
		t.Fatalf("got %v", body.Tasks)
	}
}
