package autowork

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/iodesystems/yscr/source"
)

// mockAW stands up a fake autowork3 with the P1 endpoints.
func mockAW(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var posts []string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/fleet", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"threads": []map[string]any{
			{"id": "t1", "title": "Fix bug", "status": "active",
				"task_counts": map[string]int{"active": 2, "blocked": 1}, "open_decisions": 1},
		}})
	})
	mux.HandleFunc("/api/fleet/decisions", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"decisions": []map[string]any{
			{"request_id": "req-1", "thread_id": "t1", "title": "Triage", "fields": []map[string]any{
				{"key": "item-a", "prompt": "Reply?", "type": "choice", "required": true,
					"options": []map[string]any{{"value": "apply", "label": "Apply"}}},
			}},
			{"request_id": "req-2", "thread_id": "other", "title": "elsewhere"},
		}})
	})
	mux.HandleFunc("/api/threads", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "new-thread", "name": "New work"})
	})
	mux.HandleFunc("/api/threads/new-thread/messages", func(w http.ResponseWriter, r *http.Request) {
		posts = append(posts, "new-thread")
		w.WriteHeader(200)
	})
	return httptest.NewServer(mux), &posts
}

func TestList(t *testing.T) {
	srv, _ := mockAW(t)
	defer srv.Close()
	refs, err := New(srv.URL, "", nil).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].Source != "autowork" || refs[0].ID != "t1" || refs[0].Title != "Fix bug" {
		t.Fatalf("List = %+v", refs)
	}
}

func TestState(t *testing.T) {
	srv, _ := mockAW(t)
	defer srv.Close()
	st, err := New(srv.URL, "", nil).State(context.Background(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	// open_decisions > 0 dominates → awaiting user.
	if st.Status != source.StatusAwaitingUser {
		t.Errorf("status = %q; want awaiting_user", st.Status)
	}
	if len(st.Blockers) != 1 {
		t.Errorf("blockers = %v", st.Blockers)
	}
	if len(st.Pending) != 1 || st.Pending[0].ID != "req-1" {
		t.Fatalf("pending = %+v (want only req-1 for t1)", st.Pending)
	}
	f := st.Pending[0].Fields[0]
	if f.Type != source.FieldChoice || f.Key != "item-a" || len(f.Options) != 1 {
		t.Errorf("field mapping wrong: %+v", f)
	}
}

func TestActApplyDecision(t *testing.T) {
	var got map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/api/threads/t1/decisions/req-1/submit", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(map[string]any{"applied": 1, "escalated": 0, "dismissed": 1})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	res, err := New(srv.URL, "", nil).Act(context.Background(), "t1", source.Action{
		Name: "answer_questionnaire",
		Args: map[string]any{
			"questionnaire_id": "req-1",
			"answers":          map[string]any{"item-a": "apply", "item-b": "dismiss"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res, "1 applied") || !strings.Contains(res, "1 dismissed") {
		t.Errorf("result = %q", res)
	}
	// item_ids grouped by action into decisions[].
	decs, _ := got["decisions"].([]any)
	if len(decs) != 2 {
		t.Fatalf("decisions = %d; want 2 (apply, dismiss)", len(decs))
	}
	byAction := map[string]string{}
	for _, d := range decs {
		m := d.(map[string]any)
		ids := m["item_ids"].([]any)
		byAction[m["action"].(string)] = ids[0].(string)
	}
	if byAction["apply"] != "item-a" || byAction["dismiss"] != "item-b" {
		t.Errorf("grouping wrong: %v", byAction)
	}
}

func TestSpawn(t *testing.T) {
	srv, posts := mockAW(t)
	defer srv.Close()
	ref, err := New(srv.URL, "", nil).Spawn(context.Background(), source.SpawnSpec{Title: "New work", Prompt: "do the thing"})
	if err != nil {
		t.Fatal(err)
	}
	if ref.ID != "new-thread" || ref.Source != "autowork" {
		t.Fatalf("spawn ref = %+v", ref)
	}
	if len(*posts) != 1 {
		t.Errorf("expected 1 first-message POST, got %d", len(*posts))
	}
}
