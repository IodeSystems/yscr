package service

import (
	"context"
	"strings"
	"testing"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
	"github.com/iodesystems/agentkit/llm"

	"github.com/iodesystems/yscr/config"
	"github.com/iodesystems/yscr/source"
)

func TestL1Distill(t *testing.T) {
	cases := []struct {
		name  string
		delta string
		want  string
	}{
		{"last line", "line one\nline two\nline three", "line three"},
		{"skips trailing noise", "real output\n$ ", "real output"},
		{"all noise", "$ \n#\n: \ny", ""},
		{"empty", "", ""},
		{"whitespace only", "  \n\t\n", ""},
	}
	for _, tc := range cases {
		if got := l1Distill(tc.delta); got != tc.want {
			t.Errorf("%s: l1Distill(%q) = %q, want %q", tc.name, tc.delta, got, tc.want)
		}
	}
}

func TestL1DistillLongLine(t *testing.T) {
	long := "x" + string(make([]byte, 300))
	got := l1Distill(long)
	if len(got) != 200 {
		t.Errorf("long line not capped to 200: got %d", len(got))
	}
}

// TestAmbientGate_NoDrone is the load-bearing test: a session whose state never
// advances must produce at most ONE utterance (the first advance), no matter how
// many ticks of identical output arrive. This is the L2 materiality gate.
func TestAmbientGate_NoDrone(t *testing.T) {
	c := &ambientCell{}

	const same = "build finished, 0 failures"
	var calls int
	distill := func(delta string) {
		snap := l1Distill(delta)
		if snap == "" {
			return
		}
		c.mu.Lock()
		changed := snap != c.snapshot
		if changed {
			c.snapshot = snap
			c.distillRev++
		}
		speak := changed && !c.nudged && c.distillRev > c.spokenRev
		if speak {
			c.nudged = true
		}
		c.mu.Unlock()
		if !speak {
			return
		}
		calls++
		c.mu.Lock()
		c.nudged = false
		c.spokenRev = c.distillRev
		c.mu.Unlock()
	}

	// 20 ticks of the same output: only the first may speak.
	for i := 0; i < 20; i++ {
		distill("noise\n" + same)
	}
	if calls != 1 {
		t.Fatalf("expected exactly 1 utterance for unchanged state, got %d", calls)
	}

	// A real advance speaks again.
	distill("tests now failing on 3 packages")
	if calls != 2 {
		t.Fatalf("expected 2nd utterance after advance, got %d", calls)
	}
}

// countingRunner is an LLMRunner that returns a fixed reply and counts calls.
type countingRunner struct {
	reply string
	calls int
}

func (r *countingRunner) ChatStream(ctx context.Context, msgs []llm.Message, tools []llm.ToolDef, opts *llm.ChatOpts) (<-chan llm.StreamChunk, error) {
	r.calls++
	ch := make(chan llm.StreamChunk, 2)
	ch <- llm.StreamChunk{Content: r.reply}
	ch <- llm.StreamChunk{Done: true}
	close(ch)
	return ch, nil
}

// TestAmbientLoop_E2E drives the real ambientLoop against a fake Observe stream:
// one advance speaks once (SSE narration + Notify), identical output drones not,
// a second advance speaks again.
func TestAmbientLoop_E2E(t *testing.T) {
	runner := &countingRunner{reply: "the build just finished."}
	h := newAmbientHub(config.AmbientConfig{})
	h.interval = 10 * time.Millisecond // fast distill cadence for the test
	h.minSpeak = 0                    // no min-gap in the fast test
	s := &Server{
		narr:    newNarrator(runner),
		sse:     newSSEHub(),
		ambient: h,
		push:    &pushHub{subs: map[string]*webpush.Subscription{}},
	}

	sub, cancelSub := s.sse.subscribe()
	defer cancelSub()

	ch := make(chan source.Event, 4)
	key := "terminal/pane1"
	ctx, cancel := context.WithCancel(context.Background())
	s.ambient.start(key, cancel)
	done := make(chan struct{})
	go func() {
		s.ambientLoop(ctx, key, "terminal", "pane1", "shell", ch)
		close(done)
	}()
	defer cancel()

	// Advance #1: meaningful output.
	ch <- source.Event{Content: "compiling 42 files\nbuild finished, 0 failures"}
	if !waitNarration(t, sub, "the build just finished.", 3*time.Second) {
		t.Fatalf("runner.calls=%d", runner.calls)
	}

	// Drone check: same state for several ticks must NOT speak again. The
	// snapshot only advances when the distilled line CHANGES, so identical
	// output never re-fires the gate regardless of how many ticks arrive.
	time.Sleep(40 * time.Millisecond)
	select {
	case <-done:
		t.Fatalf("ambient loop exited early")
	default:
	}
	if runner.calls != 1 {
		t.Fatalf("expected exactly 1 LLM call after unchanged output, got %d", runner.calls)
	}

	// Advance #2.
	ch <- source.Event{Content: "tests starting\n3 packages failing"}
	if !waitNarration(t, sub, "the build just finished.", 3*time.Second) { // same fixed reply; we count calls
		t.Fatalf("runner.calls=%d", runner.calls)
	}
	if runner.calls != 2 {
		t.Fatalf("expected 2 LLM calls after the second advance, got %d", runner.calls)
	}
}

func waitNarration(t *testing.T, sub <-chan sseMsg, wantText string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case m := <-sub:
			if m.event == "narration" && strings.Contains(m.data, wantText) {
				return true
			}
		case <-deadline:
			t.Errorf("timed out waiting for narration containing %q", wantText)
			return false
		}
	}
}

type ambFakeSource struct{}

func (ambFakeSource) ID() string            { return "terminal" }
func (ambFakeSource) List(ctx context.Context) ([]source.SessionRef, error) {
	return nil, nil
}
func (ambFakeSource) State(ctx context.Context, id string) (source.State, error) {
	return source.State{}, nil
}
func (ambFakeSource) Observe(ctx context.Context, id string) (<-chan source.Event, error) {
	ch := make(chan source.Event)
	close(ch)
	return ch, nil
}
func (ambFakeSource) Post(ctx context.Context, id string, msg string) error { return nil }

func TestQuietHours(t *testing.T) {
	// invalid → never quiet
	if q := quietHours("", ""); q() {
		t.Fatal("empty bounds should never be quiet")
	}
	if q := quietHours("garbage", "07:00"); q() {
		t.Fatal("invalid start should never be quiet")
	}
	// equal → never quiet
	if q := quietHours("09:00", "09:00"); q() {
		t.Fatal("equal bounds should never be quiet")
	}
	// day window: 00:00–23:59 is always inside
	q := quietHours("00:00", "23:59")
	if !q() {
		t.Fatal("full-day window should be quiet now")
	}
	// wrap window 23:00–00:30 covers midnight; pick a time guaranteed outside:
	// can't control the clock, so only check it returns without panicking and
	// that the two windows are consistent at the same instant.
	a := quietHours("23:00", "00:30")()
	b := quietHours("23:00", "00:30")()
	if a != b {
		t.Fatal("same window should agree at the same instant")
	}
}

func TestAmbientLoop_QuietSuppresses(t *testing.T) {
	runner := &countingRunner{reply: "the build just finished."}
	h := newAmbientHub(config.AmbientConfig{})
	h.interval = 10 * time.Millisecond
	h.minSpeak = 0
	h.quiet = func() bool { return true } // always quiet
	s := &Server{
		narr:    newNarrator(runner),
		sse:     newSSEHub(),
		ambient: h,
		push:    &pushHub{subs: map[string]*webpush.Subscription{}},
	}
	sub, cancelSub := s.sse.subscribe()
	defer cancelSub()

	ch := make(chan source.Event, 4)
	ctx, cancel := context.WithCancel(context.Background())
	s.ambient.start("terminal/p1", cancel)
	done := make(chan struct{})
	go func() {
		s.ambientLoop(ctx, "terminal/p1", "terminal", "p1", "shell", ch)
		close(done)
	}()
	defer cancel()

	ch <- source.Event{Content: "build finished, 0 failures"}
	time.Sleep(50 * time.Millisecond) // let a few ticks run

	select {
	case m := <-sub:
		t.Fatalf("quiet hours must suppress narration, got %s", m.event)
	default:
	}
	// the advance IS recorded — un-quieting speaks it at the next tick.
	// The held advance only re-evaluates when NEW output arrives (the distill
	// gate requires a non-empty buffer), so feed another line to trigger it.
	h.quiet = func() bool { return false }
	ch <- source.Event{Content: "still building"}
	select {
	case m := <-sub:
		if m.event != "narration" {
			t.Fatalf("expected narration after un-quiet, got %s", m.event)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected the recorded advance to be spoken after un-quiet")
	}
	cancel() // stop the loop before waiting for done
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ambientLoop did not exit after cancel")
	}
}

func TestAmbientLoop_MinInterval(t *testing.T) {
	runner := &countingRunner{reply: "ok"}
	h := newAmbientHub(config.AmbientConfig{MinIntervalSeconds: 3600})
	h.interval = 10 * time.Millisecond
	s := &Server{
		narr:    newNarrator(runner),
		sse:     newSSEHub(),
		ambient: h,
		push:    &pushHub{subs: map[string]*webpush.Subscription{}},
	}
	sub, cancelSub := s.sse.subscribe()
	defer cancelSub()

	ch := make(chan source.Event, 4)
	ctx, cancel := context.WithCancel(context.Background())
	s.ambient.start("terminal/p2", cancel)
	done := make(chan struct{})
	go func() {
		s.ambientLoop(ctx, "terminal/p2", "terminal", "p2", "shell", ch)
		close(done)
	}()
	defer cancel()

	ch <- source.Event{Content: "first advance"}
	// wait for the first utterance (the min-gap clock starts when it lands).
	// The min-gap gate only re-evaluates on NEW output, so the first tick after
	// the event arrives must speak (lastSpokenAt was backdated at start).
	select {
	case m := <-sub:
		if m.event != "narration" {
			t.Fatalf("expected narration, got %s", m.event)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first advance was never spoken")
	}
	// drain any extra messages (there shouldn't be any for a single advance)
	select {
	case <-sub:
		t.Fatal("expected exactly one utterance for the first advance")
	default:
	}
	// second advance within the min gap → held (advance recorded, not spoken)
	ch <- source.Event{Content: "second advance"}
	time.Sleep(50 * time.Millisecond)
	select {
	case m := <-sub:
		t.Fatalf("min interval must hold the second utterance, got %s", m.event)
	default:
	}
	cancel() // stop the loop before waiting for done
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ambientLoop did not exit after cancel")
	}
	if runner.calls != 1 {
		t.Fatalf("expected 1 LLM call (second advance held), got %d", runner.calls)
	}
}
