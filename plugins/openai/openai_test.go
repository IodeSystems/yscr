package openai

import (
	"context"
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"

	"github.com/iodesystems/yscr/source"
	"github.com/iodesystems/yscr/store"
)

// scriptRunner stands in for a corrallm/OpenRouter endpoint: it replays one
// reply per chat call. (Live wiring: New(llm.NewClient(corrallmURL, key,
// model), store.NewMem(), "").)
type scriptRunner struct {
	replies []string
	i       int
}

func (r *scriptRunner) ChatStream(_ context.Context, _ []llm.Message, _ []llm.ToolDef, _ *llm.ChatOpts) (<-chan llm.StreamChunk, error) {
	ch := make(chan llm.StreamChunk, 2)
	if r.i < len(r.replies) {
		ch <- llm.StreamChunk{Content: r.replies[r.i]}
	}
	r.i++
	ch <- llm.StreamChunk{Done: true}
	close(ch)
	return ch, nil
}

func TestSpawnPostState(t *testing.T) {
	runner := &scriptRunner{replies: []string{"researching now.", "found three options."}}
	p := New(runner, store.NewMem(), "")
	p.now = func() int64 { return 42 }

	ref, err := p.Spawn(context.Background(), source.SpawnSpec{Title: "Research", Prompt: "look into X"})
	if err != nil {
		t.Fatal(err)
	}
	if ref.Source != "openai" || ref.ID != "s1" {
		t.Fatalf("spawn ref = %+v", ref)
	}

	// State reflects the first reply as the summary; status idle (awaiting).
	st, _ := p.State(context.Background(), "s1")
	if st.Status != "idle" || !strings.Contains(st.Summary, "researching now") {
		t.Fatalf("state after spawn = %+v", st)
	}

	// Post advances the same conversation.
	if err := p.Post(context.Background(), "s1", "which is cheapest?"); err != nil {
		t.Fatal(err)
	}
	st, _ = p.State(context.Background(), "s1")
	if !strings.Contains(st.Summary, "three options") {
		t.Fatalf("state after post = %+v", st)
	}

	// List shows the one managed session.
	refs, _ := p.List(context.Background())
	if len(refs) != 1 || refs[0].ID != "s1" {
		t.Fatalf("list = %+v", refs)
	}
}

// fakeStore is a minimal durable store for the restore test: a fixed set of
// session logs, addressable by SessionIDs + Context.
type fakeStore struct {
	logs map[string][]agent.Entry
}

func (f *fakeStore) SessionIDs(_ context.Context) ([]string, error) {
	var out []string
	for id := range f.logs {
		out = append(out, id)
	}
	return out, nil
}

func (f *fakeStore) Context(_ context.Context, sessionID string) ([]agent.Entry, error) {
	return f.logs[sessionID], nil
}

// agent.Store conformance: the restore path only reads, but New takes a full
// store so Post can append.
func (f *fakeStore) ClaimPending(_ context.Context, _ string, _ int64) (int, error) { return 0, nil }
func (f *fakeStore) Append(_ context.Context, sessionID string, e agent.Entry) error {
	f.logs[sessionID] = append(f.logs[sessionID], e)
	return nil
}
func (f *fakeStore) Compact(_ context.Context, _ string, _ agent.Compaction) error { return nil }

func TestRestoreFromStore(t *testing.T) {
	st := &fakeStore{logs: map[string][]agent.Entry{
		"s1": {
			{Kind: agent.KindUser, Content: "look into the build failure", CreatedAt: 10},
			{Kind: agent.KindAssistant, Content: "the build fails on a missing dep.", CreatedAt: 20},
		},
		"s2": {
			{Kind: agent.KindUser, Content: "summarize the logs", CreatedAt: 30},
		},
	}}
	p := New(&scriptRunner{}, st, "")
	p.now = func() int64 { return 99 }
	p.RestoreFromStore(context.Background(), st)

	refs, err := p.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 {
		t.Fatalf("expected 2 restored sessions, got %d", len(refs))
	}
	st1, err := p.State(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	if st1.Status != source.StatusIdle {
		t.Errorf("restored session should be idle, got %v", st1.Status)
	}
	if st1.Summary != "the build fails on a missing dep." {
		t.Errorf("summary = %q", st1.Summary)
	}
	if st1.Ref.Title != "look into the build failure" {
		t.Errorf("title = %q", st1.Ref.Title)
	}

	// Post to a restored session re-enters through turn() and appends to the
	// same log.
	p2 := New(&scriptRunner{replies: []string{"still looking."}}, st, "")
	p2.now = func() int64 { return 99 }
	p2.RestoreFromStore(context.Background(), st)
	if err := p2.Post(context.Background(), "s1", "any progress?"); err != nil {
		t.Fatal(err)
	}
	log := st.logs["s1"]
	// original 2 + the new user message + the assistant reply = 4
	if len(log) != 4 {
		t.Fatalf("expected the restored log to grow to 4 entries, got %d", len(log))
	}
}

func TestRestoreSkipsKnown(t *testing.T) {
	st := &fakeStore{logs: map[string][]agent.Entry{
		"s1": {{Kind: agent.KindUser, Content: "old prompt", CreatedAt: 5}},
	}}
	p := New(&scriptRunner{}, st, "")
	p.sess["s1"] = &meta{id: "s1", title: "live one", status: source.StatusRunning}
	p.RestoreFromStore(context.Background(), st)
	if p.sess["s1"].title != "live one" {
		t.Fatalf("restore must not clobber a live session, got %q", p.sess["s1"].title)
	}
}
