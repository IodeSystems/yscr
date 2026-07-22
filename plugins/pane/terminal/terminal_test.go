package terminal

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/iodesystems/yscr/plugins/pane"
	"github.com/iodesystems/yscr/source"
)

// fakeTmux implements pane.Tmux. `live` maps a session id to its target;
// scrollback/capture are canned; SendKeys is recorded.
type fakeTmux struct {
	calls      [][]string
	live       map[string]string
	capture    string
	scrollback string
}

func (f *fakeTmux) Target(_ context.Context, s pane.Session) (string, bool) {
	if t, ok := f.live[s.ID]; ok {
		return t, true
	}
	return "yscr-cc-" + s.ID, false
}
func (f *fakeTmux) Capture(context.Context, string) (string, error) { return f.capture, nil }
func (f *fakeTmux) Scrollback(context.Context, string, int) (string, error) {
	return f.scrollback, nil
}
func (f *fakeTmux) SendKeys(_ context.Context, target string, keys ...string) error {
	f.calls = append(f.calls, append([]string{"send-keys", "-t", target}, keys...))
	return nil
}
func (f *fakeTmux) Launch(context.Context, pane.Session, string, []string) (string, error) {
	return "", nil
}

func newT() *Adapter {
	a := New(Config{})
	a.now = func() int64 { return 7 }
	return a
}

// Adopt claims a normal-screen pane; declines an alt-screen TUI.
func TestAdopt_GatesOnAltScreen(t *testing.T) {
	a := newT()
	if s, ok := a.Adopt(pane.LivePane{Target: "%2", Program: "fish", Pid: 2002, Alt: false}); !ok {
		t.Errorf("normal-screen pane should adopt; got %+v ok=%v", s, ok)
	} else if s.ID != "%2" || s.Source != SourceID {
		t.Errorf("adopted session = %+v", s)
	}
	if _, ok := a.Adopt(pane.LivePane{Target: "%3", Program: "vim", Pid: 3003, Alt: true}); ok {
		t.Error("alt-screen TUI should be declined (no scrollback, captures input)")
	}
}

func TestHandles_ExcludesClaude(t *testing.T) {
	a := newT()
	if a.Handles("claude") {
		t.Error("claude is the claude adapter's; terminal must not handle it")
	}
	if !a.Handles("fish") || !a.Handles("go") {
		t.Error("terminal should handle line-oriented programs")
	}
}

func TestDiscover_Empty(t *testing.T) {
	if got := newT().Discover(context.Background()); got != nil {
		t.Errorf("terminal has no persistent sessions; got %+v", got)
	}
}

func TestState_ShellIdleRunningElse(t *testing.T) {
	a := newT()
	f := &fakeTmux{live: map[string]string{"%2": "work:1.0"}, capture: "$ ls\nfile.go\n$ "}
	st, err := a.State(context.Background(), pane.Session{Source: SourceID, ID: "%2", Program: "fish"}, f)
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != source.StatusIdle {
		t.Errorf("shell should be idle; got %q", st.Status)
	}
	if !strings.Contains(st.Summary, "file.go") {
		t.Errorf("summary from pane viewport = %q", st.Summary)
	}
	// A running program (not a shell) → running.
	st2, _ := a.State(context.Background(), pane.Session{Source: SourceID, ID: "%2", Program: "go"}, f)
	if st2.Status != source.StatusRunning {
		t.Errorf("non-shell program should be running; got %q", st2.Status)
	}
}

func TestHistory_FromScrollback(t *testing.T) {
	a := newT()
	f := &fakeTmux{live: map[string]string{"%2": "work:1.0"}, scrollback: "line 1\nline 2\nline 3\n\n"}
	got, err := a.History(context.Background(), pane.Session{ID: "%2"}, 40, f)
	if err != nil {
		t.Fatal(err)
	}
	if got != "line 1\nline 2\nline 3" {
		t.Errorf("history = %q", got)
	}
}

func TestHistory_DeadPaneUnsupported(t *testing.T) {
	a := newT()
	f := &fakeTmux{} // not live
	if _, err := a.History(context.Background(), pane.Session{ID: "%9"}, 40, f); !errors.Is(err, source.ErrUnsupported) {
		t.Errorf("dead pane history err = %v; want ErrUnsupported", err)
	}
}

func TestPost_SendsToLivePane(t *testing.T) {
	a := newT()
	f := &fakeTmux{live: map[string]string{"%2": "work:1.0"}}
	if err := a.Post(context.Background(), pane.Session{ID: "%2"}, "echo hi", f); err != nil {
		t.Fatal(err)
	}
	sent := false
	enter := false
	for _, c := range f.calls {
		j := strings.Join(c, " ")
		if strings.Contains(j, "-l echo hi") {
			sent = true
		}
		if strings.HasSuffix(j, "Enter") {
			enter = true
		}
	}
	if !sent || !enter {
		t.Errorf("want message + Enter to work:1.0; calls = %v", f.calls)
	}
}

func TestSpawnAct_Unsupported(t *testing.T) {
	a := newT()
	if _, err := a.Spawn(context.Background(), source.SpawnSpec{}, &fakeTmux{}); !errors.Is(err, source.ErrUnsupported) {
		t.Errorf("Spawn err = %v; want ErrUnsupported", err)
	}
	if _, err := a.Act(context.Background(), pane.Session{}, source.Action{}, &fakeTmux{}); !errors.Is(err, source.ErrUnsupported) {
		t.Errorf("Act err = %v; want ErrUnsupported", err)
	}
}
