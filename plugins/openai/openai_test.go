package openai

import (
	"context"
	"strings"
	"testing"

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
