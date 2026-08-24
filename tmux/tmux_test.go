package tmux

import (
	"context"
	"sync"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/yscr/terminal"
)

// fakeTmux scripts tmux responses. listPanes is the tree; captures maps a
// target to its screen text; sends records send-keys calls; spawns records
// spawn calls and appends a pane to the tree.
type fakeTmux struct {
	listPanes string
	captures  map[string]string
	sends     [][]string
	spawns    [][]string
	exec      func(_ context.Context, _ string, args ...string) (string, error) // nil → fakeTmuxExec
}

func fakeTmuxExec(f *fakeTmux, args ...string) (string, error) {
	switch args[0] {
	case "list-panes":
		return f.listPanes, nil
	case "capture-pane":
		t := args[2]
		if v, ok := f.captures[t]; ok {
			return v, nil
		}
		return "", fmt.Errorf("no capture for %s", t)
	case "send-keys":
		f.sends = append(f.sends, args)
		return "", nil
	case "has-session":
		// The session exists in the test tree; no create needed.
		return "", nil
	case "new-session":
		f.spawns = append(f.spawns, args)
		return "", nil
	case "new-window", "split-window":
		f.spawns = append(f.spawns, args)
		return "", nil
	}
	return "", fmt.Errorf("unexpected tmux %s", args[0])
}

// The scan format is 9 tab-separated fields:
// pane_id, session_name, window_index, pane_index, pane_pid, program, tty, alt, active.
const tree = "%1\twork\t0\t0\t111\tsh\t/dev/pts/1\t0\t1\n" +
	"%2\twork\t1\t0\t222\tclaude\t/dev/pts/2\t1\t0\n" // alt-screen pane

func newTestActivity(f *fakeTmux) *Activity {
	a := New(Config{KeyframeInterval: 5 * time.Millisecond})
	a.linePoll = 5 * time.Millisecond
	fake := f
	if fake.exec != nil {
		a.exec = fake.exec
	} else {
		a.exec = func(_ context.Context, _ string, args ...string) (string, error) {
			return fakeTmuxExec(fake, args...)
		}
	}
	a.now = func() int64 { return time.Now().UnixNano() }
	return a
}

// ── tree ────────────────────────────────────────────────────────────

func TestScan_ParsesTree(t *testing.T) {
	f := &fakeTmux{listPanes: tree}
	a := newTestActivity(f)
	panes, err := a.Scan(context.Background())
	if err != nil || len(panes) != 2 {
		t.Fatalf("scan = %d, %v", len(panes), err)
	}
	p0, p1 := panes[0], panes[1]
	if p0.ID != "%1" || p0.Session != "work" || p0.Window != 0 || p0.Program != "sh" || p0.Target != "work:0.0" {
		t.Errorf("p0 = %+v", p0)
	}
	if !p1.Alt || p1.Program != "claude" || p1.Target != "work:1.0" {
		t.Errorf("p1 = %+v", p1)
	}
	if KindOf(p0) != terminal.Lines || KindOf(p1) != terminal.Frames {
		t.Errorf("kinds = %v %v", KindOf(p0), KindOf(p1))
	}
}

func TestPaneByID(t *testing.T) {
	f := &fakeTmux{listPanes: tree}
	a := newTestActivity(f)
	p, err := a.PaneByID(context.Background(), "%2")
	if err != nil || p.Program != "claude" {
		t.Fatalf("PaneByID = %+v, %v", p, err)
	}
	if _, err := a.PaneByID(context.Background(), "%9"); err == nil {
		t.Error("missing pane should error")
	}
}

// ── yielding + feeding sessions ─────────────────────────────────────

func TestSessionFor_SeedsAndFeedsLines(t *testing.T) {
	f := &fakeTmux{listPanes: tree, captures: map[string]string{"work:0.0": "seed1\nseed2"}}
	a := newTestActivity(f)
	p, _ := a.PaneByID(context.Background(), "%1")

	s := a.SessionFor(context.Background(), p) // seeds from the capture
	segs := s.Segments()
	if len(segs) != 1 || segs[0].Program != "sh" || segs[0].Kind != terminal.Lines {
		t.Fatalf("seeded = %+v", segs)
	}
	if strings.Join(segs[0].Lines, "|") != "seed1|seed2" {
		t.Errorf("seed lines = %q", segs[0].Lines)
	}

	// New output appears → the feed appends it (watermark set at seed time).
	f.captures["work:0.0"] = "seed1\nseed2\nnew line"
	a.PollPane(context.Background(), p)
	segs = s.Segments()
	if strings.Join(segs[0].Lines, "|") != "seed1|seed2|new line" {
		t.Errorf("after poll = %q", segs[0].Lines)
	}
	// Polling again with no change adds nothing.
	a.PollPane(context.Background(), p)
	if got := len(s.Segments()[0].Lines); got != 3 {
		t.Errorf("no-change poll added lines: %d", got)
	}
}

func TestSessionFor_ProgramChangeSegments(t *testing.T) {
	f := &fakeTmux{listPanes: tree, captures: map[string]string{"work:0.0": "shell out"}}
	a := newTestActivity(f)
	p, _ := a.PaneByID(context.Background(), "%1")
	s := a.SessionFor(context.Background(), p)

	// The pane's foreground program flips to vim (alt screen).
	f.listPanes = strings.Replace(tree, "%1\twork\t0\t0\t111\tsh\t/dev/pts/1\t0\t1", "%1\twork\t0\t0\t333\tvim\t/dev/pts/1\t1\t1", 1)
	p.Program, p.Alt = "vim", true
	a.PollPane(context.Background(), p)

	segs := s.Segments()
	if len(segs) != 2 || segs[0].Program != "sh" || segs[1].Program != "vim" || segs[1].Kind != terminal.Frames {
		t.Fatalf("segments = %+v", segs)
	}
	if segs[0].End == 0 {
		t.Error("closed segment must carry an End")
	}
}

func TestSessionFor_FramesPaneCapturesKeyframes(t *testing.T) {
	f := &fakeTmux{listPanes: tree, captures: map[string]string{"work:1.0": "frame A"}}
	a := newTestActivity(f)
	p, _ := a.PaneByID(context.Background(), "%2")
	s := a.SessionFor(context.Background(), p) // seeds frame A

	f.captures["work:1.0"] = "frame B"
	a.PollPane(context.Background(), p) // not streaming → poll captures a keyframe
	segs := s.Segments()
	if len(segs[0].Frames) != 2 {
		t.Fatalf("frames = %d", len(segs[0].Frames))
	}
}

// ── act ─────────────────────────────────────────────────────────────

func TestSendKeys_LiteralLinesPlusEnter(t *testing.T) {
	f := &fakeTmux{listPanes: tree}
	a := newTestActivity(f)
	if err := a.SendKeys(context.Background(), "%1", "echo hi\necho there"); err != nil {
		t.Fatal(err)
	}
	if len(f.sends) != 1 {
		t.Fatalf("sends = %v", f.sends)
	}
	want := []string{"send-keys", "-t", "work:0.0", "-l", "echo hi", "Enter", "-l", "echo there", "Enter"}
	if strings.Join(f.sends[0], "|") != strings.Join(want, "|") {
		t.Errorf("args = %v", f.sends[0])
	}
}

func TestSpawn_NewWindow(t *testing.T) {
	f := &fakeTmux{listPanes: tree}
	a := newTestActivity(f)
	// The re-scan after spawn still shows only the old tree → resolution fails;
	// we assert on the tmux args, which is what matters.
	id, err := a.Spawn(context.Background(), SpawnSpec{Session: "work", Dir: "/tmp", Argv: []string{"bash"}})
	if err == nil && id == "" {
		t.Fatal("spawn returned neither an id nor an error")
	}
	// has-session succeeded → the first spawn record is new-window itself.
	args := f.spawns[0]
	if args[0] != "new-window" || !contains(args, "-c") || !contains(args, "/tmp") || !contains(args, "bash") {
		t.Errorf("spawn args = %v", args)
	}
}

func TestSpawn_Split(t *testing.T) {
	f := &fakeTmux{listPanes: tree}
	a := newTestActivity(f)
	_, _ = a.Spawn(context.Background(), SpawnSpec{Session: "work", Split: true, Argv: []string{"htop"}})
	if len(f.spawns) != 1 || f.spawns[0][0] != "split-window" {
		t.Fatalf("expected one split-window spawn, got %v", f.spawns)
	}
}

// ── stream ──────────────────────────────────────────────────────────

func TestStream_LinesEmitsNewOnly(t *testing.T) {
	fake := &fakeTmux{listPanes: tree, captures: map[string]string{"work:0.0": "h1\nh2\nh3\nl1\nl2"}}
	var mu sync.Mutex
	fake.exec = func(_ context.Context, _ string, args ...string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		return fakeTmuxExec(fake, args...)
	}
	a := newTestActivity(fake)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := a.Stream(ctx, "%1")
	if err != nil {
		t.Fatal(err)
	}
	// The stream's baseline is the FULL tail at start (h1..l2); only genuinely
	// new lines are emitted. Append from a GOROUTINE: appending inline would
	// block the select until an event exists, which can never happen — the
	// append is what creates it.
	go func() {
		for i := 3; ; i++ {
			time.Sleep(10 * time.Millisecond)
			mu.Lock()
			fake.captures["work:0.0"] += fmt.Sprintf("\nl%d", i)
			mu.Unlock()
		}
	}()
	var got []string
		for len(got) < 2 {
		select {
		case ev := <-ch:
			got = append(got, ev.Content)
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out; got %v so far (captures=%q wm=%d streaming=%v)", got, fake.captures["work:0.0"], a.wm["%1"], a.streaming)
		}
	}
	if got[0] != "l3" || got[1] != "l4" {
		t.Errorf("streamed = %v (want only NEW lines)", got)
	}
}

func TestStream_FramesEmitsDiffs(t *testing.T) {
	// The stream's first frame is a fresh capture (the seed is not its baseline),
	// so make the seed and the first frame identical → the second tick's diff is
	// exactly the change we introduce.
	fake := &fakeTmux{listPanes: tree, captures: map[string]string{"work:1.0": "A\nsame"}}
	var mu sync.Mutex
	fake.exec = func(_ context.Context, _ string, args ...string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		return fakeTmuxExec(fake, args...)
	}
	a := newTestActivity(fake)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := a.Stream(ctx, "%2")
	if err != nil {
		t.Fatal(err)
	}
	// Mutate from a GOROUTINE: the select below only unblocks once an event
	// exists, and the mutation is what creates it — doing it inline would block
	// on the mutex the stream goroutine holds during its first tick.
	go func() {
		time.Sleep(10 * time.Millisecond)
		mu.Lock()
		fake.captures["work:1.0"] = "B\nsame" // middle line changes → diff
		mu.Unlock()
	}()
	select {
	case e := <-ch:
		c := e.Content
		if !strings.Contains(c, "+B") || strings.Contains(c, "+same") {
			t.Errorf("frame diff = %q", c)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for first frame diff (captures=%v)", fake.captures)
	}
	// The pane's session was fed too.
	s := a.SessionFor(context.Background(), mustPanic(t, a))
	if len(s.Segments()[0].Frames) < 2 {
		t.Errorf("session not fed by stream: %+v", s.Segments())
	}
}

// ── history through the model ───────────────────────────────────────

func TestHistory_QueryThroughSession(t *testing.T) {
	f := &fakeTmux{listPanes: tree, captures: map[string]string{"work:0.0": "a\nb\nc"}}
	a := newTestActivity(f)
	p, _ := a.PaneByID(context.Background(), "%1")
	s := a.SessionFor(context.Background(), p)

	out := s.QueryHistory(terminal.Query{Tail: 2})
	if out != "b\nc" {
		t.Errorf("tail = %q", out)
	}
	out = s.QueryHistory(terminal.Query{Grep: "B"}) // case-insensitive
	if out != "b" {
		t.Errorf("grep = %q", out)
	}
}

// ── helpers ─────────────────────────────────────────────────────────

func contains(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// mustPanic returns a pane (used only to satisfy SessionFor in the stream test).
func mustPanic(t *testing.T, a *Activity) Pane {
	t.Helper()
	p, err := a.PaneByID(context.Background(), "%2")
	if err != nil {
		t.Fatal(err)
	}
	return p
}
