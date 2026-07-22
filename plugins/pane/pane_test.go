package pane

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/iodesystems/yscr/source"
)

// stubAdapter is a minimal Adapter for exercising the Source shell (List merge,
// find/routing, Observe). It records which sessions it was asked about.
type stubAdapter struct {
	id        string
	handles   string
	discover  []Session
	adopt     bool // implement Adopter?
	adoptSess Session
}

func (s *stubAdapter) ID() string                         { return s.id }
func (s *stubAdapter) Handles(prog string) bool           { return prog == s.handles }
func (s *stubAdapter) Discover(context.Context) []Session { return s.discover }
func (s *stubAdapter) State(_ context.Context, ss Session, _ Tmux) (source.State, error) {
	return source.State{Ref: source.SessionRef{Source: s.id, ID: ss.ID}, Summary: "sum:" + ss.ID}, nil
}
func (s *stubAdapter) History(_ context.Context, ss Session, _ int, _ Tmux) (string, error) {
	return "hist:" + ss.ID, nil
}
func (s *stubAdapter) Post(context.Context, Session, string, Tmux) error { return nil }
func (s *stubAdapter) Spawn(_ context.Context, spec source.SpawnSpec, _ Tmux) (Session, error) {
	return Session{Source: s.id, ID: "spawned", Cwd: spec.Dir}, nil
}
func (s *stubAdapter) Act(context.Context, Session, source.Action, Tmux) (string, error) {
	return "acted", nil
}

// adoptStub adds the Adopter capability.
type adoptStub struct{ *stubAdapter }

func (a adoptStub) Adopt(p LivePane) (Session, bool) {
	return Session{Source: a.id, ID: "adopted-" + p.Target, Program: p.Program, Pid: p.Pid}, true
}

func newFakeSource(ad Adapter, fake func(ctx context.Context, name string, args ...string) (string, error)) *Source {
	t := newTmux("tmux", "yscr-cc")
	t.exec = fake
	s := newWith(ad, t, 25)
	s.now = func() int64 { return 7 }
	return s
}

func TestSource_ListFromDiscover(t *testing.T) {
	ad := &stubAdapter{id: "claude-code", handles: "claude", discover: []Session{
		{ID: "a", Cwd: "/x/alpha", UpdatedAt: 200},
		{ID: "b", Cwd: "/x/beta", UpdatedAt: 100},
	}}
	s := newFakeSource(ad, func(context.Context, string, ...string) (string, error) { return "", nil })
	refs, err := s.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 || refs[0].ID != "a" { // newest (updatedAt 200) first
		t.Fatalf("refs = %+v", refs)
	}
	if refs[0].Source != "claude-code" || refs[0].Title != "alpha" {
		t.Errorf("ref[0] = %+v", refs[0])
	}
}

// A stateless adapter (Adopter) materializes sessions from live panes it Handles,
// skipping any pane already covered by a discovered session's pid.
func TestSource_AdoptsLivePanes(t *testing.T) {
	base := &stubAdapter{id: "shell", handles: "fish", discover: []Session{
		{ID: "known", Pid: 1001, UpdatedAt: 50}, // already covers pid 1001
	}}
	ad := adoptStub{base}
	scan := "%1\t1001\tfish\t/dev/pts/1\t0\n" + // pid 1001 → covered, skip
		"%2\t2002\tfish\t/dev/pts/2\t0\n" + // fish, uncovered → adopt
		"%3\t3003\tvim\t/dev/pts/3\t1\n" // not handled → ignore
	s := newFakeSource(ad, func(_ context.Context, _ string, args ...string) (string, error) {
		if args[0] == "list-panes" {
			return scan, nil
		}
		return "", nil
	})
	refs, err := s.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, r := range refs {
		ids[r.ID] = true
	}
	if !ids["known"] || !ids["adopted-%2"] {
		t.Fatalf("want known + adopted-%%2; got %+v", ids)
	}
	if ids["adopted-%1"] {
		t.Error("pid 1001 was already covered by discover; should not re-adopt")
	}
	if len(refs) != 2 {
		t.Errorf("refs = %+v; want 2", refs)
	}
}

func TestSource_RoutesStateAndHistory(t *testing.T) {
	ad := &stubAdapter{id: "claude-code", handles: "claude", discover: []Session{{ID: "a"}}}
	s := newFakeSource(ad, func(context.Context, string, ...string) (string, error) { return "", nil })
	st, err := s.State(context.Background(), "a")
	if err != nil || st.Summary != "sum:a" {
		t.Fatalf("state = %+v err=%v", st, err)
	}
	h, err := s.History(context.Background(), "a", 5)
	if err != nil || h != "hist:a" {
		t.Fatalf("history = %q err=%v", h, err)
	}
}

func TestSource_Observe(t *testing.T) {
	ad := &stubAdapter{id: "claude-code", handles: "claude", discover: []Session{{ID: "a"}}}
	s := newFakeSource(ad, func(context.Context, string, ...string) (string, error) { return "", nil })
	ch, err := s.Observe(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	ev, ok := <-ch
	if !ok || ev.Content != "sum:a" {
		t.Errorf("event = %+v ok=%v", ev, ok)
	}
	if _, more := <-ch; more {
		t.Error("Observe should emit once then close")
	}
}

// ── tmux plumbing: pid↔tty↔pane join + scan ─────────────────────────

func TestTmux_TargetJoinsOwnPane(t *testing.T) {
	// has-session fails (not our window); the pid→tty→pane join finds the user's.
	d := newTmux("tmux", "yscr-cc")
	d.ttyOf = func(pid int) string { return fmt.Sprintf("/dev/pts/%d", pid) }
	d.exec = func(_ context.Context, _ string, args ...string) (string, error) {
		switch args[0] {
		case "has-session":
			return "", fmt.Errorf("no session")
		case "list-panes":
			return "/dev/pts/1001\twork:2.1\n/dev/pts/9\telse:0.0\n", nil
		}
		return "", nil
	}
	tgt, live := d.Target(context.Background(), Session{ID: "sess-A", Pid: 1001})
	if !live || tgt != "work:2.1" {
		t.Fatalf("target = %q live=%v; want work:2.1", tgt, live)
	}
}

func TestTmux_TargetNotLiveWhenPidDead(t *testing.T) {
	d := newTmux("tmux", "yscr-cc")
	d.ttyOf = func(int) string { return "" } // dead
	d.exec = func(_ context.Context, _ string, args ...string) (string, error) {
		if args[0] == "has-session" {
			return "", fmt.Errorf("no session")
		}
		return "", nil
	}
	if _, live := d.Target(context.Background(), Session{ID: "sess-A", Pid: 1001}); live {
		t.Error("dead pid should not be live")
	}
}

func TestTmux_Scan(t *testing.T) {
	d := newTmux("tmux", "yscr-cc")
	d.exec = func(_ context.Context, _ string, args ...string) (string, error) {
		if args[0] == "list-panes" {
			return "%1\t1001\tclaude\t/dev/pts/1\t1\n%2\t2002\tfish\t/dev/pts/2\t0\n", nil
		}
		return "", nil
	}
	panes := d.scan(context.Background())
	if len(panes) != 2 || panes[0].Program != "claude" || panes[0].Pid != 1001 {
		t.Fatalf("scan = %+v", panes)
	}
	if panes[1].Target != "%2" || panes[1].TTY != "/dev/pts/2" {
		t.Errorf("pane[1] = %+v", panes[1])
	}
}

func TestTmux_LaunchReturnsTarget(t *testing.T) {
	d := newTmux("tmux", "yscr-cc")
	var got []string
	d.exec = func(_ context.Context, _ string, args ...string) (string, error) {
		got = args
		return "", nil
	}
	tgt, err := d.Launch(context.Background(), Session{ID: "sid1"}, "/dir", []string{"claude", "--resume", "sid1"})
	if err != nil || tgt != "yscr-cc-sid1" {
		t.Fatalf("launch target = %q err=%v", tgt, err)
	}
	if strings.Join(got, " ") != "new-session -d -s yscr-cc-sid1 -x 220 -y 50 -c /dir claude --resume sid1" {
		t.Errorf("launch args = %v", got)
	}
}
