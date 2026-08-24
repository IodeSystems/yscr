package concierge

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/iodesystems/agentkit/llm"

	"github.com/iodesystems/yscr/store"
)

// flakyRunner fails the first N attempts with a mid-stream death (the fault
// class that is transient AND not resumable inside the client), then succeeds.
type flakyRunner struct {
	fails   int
	calls   int
	latency time.Duration // per-attempt sleep, to simulate queue/generation time
}

func (r *flakyRunner) ChatStream(_ context.Context, _ []llm.Message, _ []llm.ToolDef, _ *llm.ChatOpts) (<-chan llm.StreamChunk, error) {
	r.calls++
	if r.latency > 0 {
		time.Sleep(r.latency)
	}
	if r.calls <= r.fails {
		ch := make(chan llm.StreamChunk, 1)
		// A mid-stream death: the body closes without [DONE]. The client
		// reports it as a stream error; "unexpected EOF" is the transient
		// signature TransientUpstream matches.
		ch <- llm.StreamChunk{Error: "stream error: unexpected EOF"}
		close(ch)
		return ch, nil
	}
	ch := make(chan llm.StreamChunk, 2)
	ch <- llm.StreamChunk{Content: "recovered"}
	ch <- llm.StreamChunk{Done: true}
	close(ch)
	return ch, nil
}

func TestRunTurnWithRetry_RetriesMidStreamDeath(t *testing.T) {
	old := sleepOrCancel
	sleepOrCancel = func(_ context.Context, _ time.Duration) bool { return true } // no real waiting
	defer func() { sleepOrCancel = old }()

	r := &flakyRunner{fails: 2}
	c := New(r, store.NewMem())
	reply, err := c.runTurnWithRetry(context.Background(), "s", "hello")
	if err != nil {
		t.Fatalf("expected recovery, got: %v", err)
	}
	if reply != "recovered" {
		t.Errorf("reply = %q, want %q", reply, "recovered")
	}
	if r.calls != 3 {
		t.Errorf("attempts = %d, want 3 (2 failures + 1 success)", r.calls)
	}
}

// A non-transient failure must NOT be retried: the caller's problem stays
// visible on attempt 1.
func TestRunTurnWithRetry_NoRetryOnLocalFailure(t *testing.T) {
	old := sleepOrCancel
	sleepOrCancel = func(_ context.Context, _ time.Duration) bool { return true }
	defer func() { sleepOrCancel = old }()

	r := &failRunner{err: errors.New("invalid arguments for tool")}
	c := New(r, store.NewMem())
	if _, err := c.runTurnWithRetry(context.Background(), "s", "hello"); err == nil {
		t.Fatal("expected an error")
	}
	if r.calls != 1 {
		t.Errorf("attempts = %d, want 1 (non-transient must not retry)", r.calls)
	}
}

// A cancelled parent ctx is fatal even when the failure looks transient.
func TestRunTurnWithRetry_StopsOnCancel(t *testing.T) {
	old := sleepOrCancel
	sleepOrCancel = func(_ context.Context, _ time.Duration) bool { return true }
	defer func() { sleepOrCancel = old }()

	r := &flakyRunner{fails: 10}
	c := New(r, store.NewMem())
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	// Let the first attempt run out its (tiny) window so the ctx dies while we
	// would otherwise be retrying.
	time.Sleep(60 * time.Millisecond)
	if _, err := c.runTurnWithRetry(ctx, "s", "hello"); err == nil {
		t.Fatal("expected the cancelled ctx to stop the loop")
	}
}

type failRunner struct {
	err   error
	calls int
}

func (r *failRunner) ChatStream(_ context.Context, _ []llm.Message, _ []llm.ToolDef, _ *llm.ChatOpts) (<-chan llm.StreamChunk, error) {
	r.calls++
	ch := make(chan llm.StreamChunk, 1)
	ch <- llm.StreamChunk{Error: r.err.Error()}
	close(ch)
	return ch, nil
}
