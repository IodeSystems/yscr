package concierge

import (
	"context"
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/llm"

	"github.com/iodesystems/yscr/source"
	"github.com/iodesystems/yscr/store"
)

// fakeSource is a canned source.Source for the tool-loop test.
type fakeSource struct {
	listed int
}

func (f *fakeSource) ID() string { return "fake" }
func (f *fakeSource) List(context.Context) ([]source.SessionRef, error) {
	f.listed++
	return []source.SessionRef{{Source: "fake", ID: "s1", Title: "Ship it"}}, nil
}
func (f *fakeSource) State(_ context.Context, id string) (source.State, error) {
	return source.State{
		Ref:     source.SessionRef{Source: "fake", ID: id, Title: "Ship it"},
		Status:  source.StatusRunning,
		Summary: "1 active task",
	}, nil
}
func (f *fakeSource) Observe(context.Context, string) (<-chan source.Event, error) { return nil, nil }
func (f *fakeSource) Post(context.Context, string, string) error                   { return nil }

// scriptRunner replays canned chat turns.
type scriptRunner struct {
	turns [][]llm.StreamChunk
	i     int
}

func (r *scriptRunner) ChatStream(_ context.Context, _ []llm.Message, _ []llm.ToolDef, _ *llm.ChatOpts) (<-chan llm.StreamChunk, error) {
	var chunks []llm.StreamChunk
	if r.i < len(r.turns) {
		chunks = r.turns[r.i]
	}
	r.i++
	ch := make(chan llm.StreamChunk, len(chunks)+1)
	for _, c := range chunks {
		ch <- c
	}
	ch <- llm.StreamChunk{Done: true}
	close(ch)
	return ch, nil
}

func fleetStatusCall() *llm.ToolCall {
	tc := &llm.ToolCall{ID: "call-1", Type: "function"}
	tc.Function.Name = "fleet_status"
	tc.Function.Arguments = "{}"
	return tc
}

// TestConverse_DrivesSource — a user message makes the concierge call
// fleet_status (which lists the source), then reply. Proves the agentkit tool
// loop dispatches into the source contract.
func TestConverse_DrivesSource(t *testing.T) {
	fs := &fakeSource{}
	runner := &scriptRunner{turns: [][]llm.StreamChunk{
		{{ToolCall: fleetStatusCall()}}, // turn 1: call the tool
		{{Content: "One task running: Ship it."}}, // turn 2: reply
	}}
	c := New(runner, store.NewMem(), fs)

	reply, err := c.Converse(context.Background(), "sess-1", "what's going on?")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "Ship it") {
		t.Fatalf("reply = %q", reply)
	}
	if fs.listed == 0 {
		t.Error("fleet_status did not drive the source's List")
	}
}
