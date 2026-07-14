package concierge

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/agentkit/llm"

	"github.com/iodesystems/yscr/store"
)

// gateRunner records the last user message of each turn and blocks the FIRST
// turn on a gate so a test can enqueue more work mid-turn.
type gateRunner struct {
	users   []string      // last user content per turn, in call order (worker is single-threaded)
	n       int
	started chan struct{} // closed when the first turn enters ChatStream
	gate    chan struct{} // the first turn blocks here until closed
}

func (r *gateRunner) ChatStream(_ context.Context, msgs []llm.Message, _ []llm.ToolDef, _ *llm.ChatOpts) (<-chan llm.StreamChunk, error) {
	idx := r.n
	r.n++
	if len(msgs) > 0 {
		r.users = append(r.users, msgs[len(msgs)-1].Content) // the just-injected user turn
	}
	if idx == 0 {
		close(r.started)
		<-r.gate // hold turn 1 "in processing" until the test releases it
	}
	ch := make(chan llm.StreamChunk, 2)
	ch <- llm.StreamChunk{Content: "reply"}
	ch <- llm.StreamChunk{Done: true}
	close(ch)
	return ch, nil
}

// TestConverse_CoalescesDuringTurn — messages that arrive while a turn is
// processing merge into ONE follow-up turn (not one racy turn each), and every
// coalesced caller gets that turn's reply. Also exercises the serialization: the
// worker is single-threaded, so users[] is race-free.
func TestConverse_CoalescesDuringTurn(t *testing.T) {
	r := &gateRunner{started: make(chan struct{}), gate: make(chan struct{})}
	c := New(r, store.NewMem())

	rep1 := make(chan string, 1)
	go func() { s, _ := c.Converse(context.Background(), "s", "A"); rep1 <- s }()
	<-r.started // turn 1 is now inside ChatStream (processing)

	// Enqueue B and C while turn 1 is blocked; they must buffer, not start turns.
	rep2 := make(chan string, 1)
	rep3 := make(chan string, 1)
	go func() { s, _ := c.Converse(context.Background(), "s", "B"); rep2 <- s }()
	go func() { s, _ := c.Converse(context.Background(), "s", "C"); rep3 <- s }()

	// Deterministically wait until both are buffered on the session channel before
	// releasing turn 1 (same-package access to the unexported queue).
	q := c.queue("s")
	deadline := time.Now().Add(2 * time.Second)
	for len(q.ch) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("B and C never enqueued")
		}
		time.Sleep(time.Millisecond)
	}

	close(r.gate) // let turn 1 finish → worker drains B+C into one merged turn

	waitRep := func(ch chan string) string {
		select {
		case s := <-ch:
			return s
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for reply")
			return ""
		}
	}
	_ = waitRep(rep1)
	r2, r3 := waitRep(rep2), waitRep(rep3)

	if len(r.users) != 2 {
		t.Fatalf("expected 2 turns (A, then merged B+C), got %d: %q", len(r.users), r.users)
	}
	if r.users[0] != "A" {
		t.Errorf("turn 1 = %q, want A", r.users[0])
	}
	if !strings.Contains(r.users[1], "B") || !strings.Contains(r.users[1], "C") {
		t.Errorf("turn 2 = %q, want both B and C merged", r.users[1])
	}
	if r2 != r3 {
		t.Errorf("coalesced callers got different replies: %q vs %q", r2, r3)
	}
}
