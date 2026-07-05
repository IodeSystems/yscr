package claudecode

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/iodesystems/yscr/source"
)

// fakeTmux records tmux invocations and returns canned output.
type fakeTmux struct {
	calls [][]string
	pane  string
	alive bool
}

func (f *fakeTmux) run(_ context.Context, _ string, args ...string) (string, error) {
	f.calls = append(f.calls, args)
	switch args[0] {
	case "capture-pane":
		return f.pane, nil
	case "has-session":
		if f.alive {
			return "", nil
		}
		return "", fmt.Errorf("no session")
	default:
		return "", nil
	}
}

func (f *fakeTmux) sawArg(want string) bool {
	for _, c := range f.calls {
		for _, a := range c {
			if a == want {
				return true
			}
		}
	}
	return false
}

func newTestPlugin(f *fakeTmux) *Plugin {
	p := New(Config{Command: []string{"claude"}})
	p.exec = f.run
	p.now = func() int64 { return 7 }
	return p
}

func TestSpawnListStatePost(t *testing.T) {
	f := &fakeTmux{pane: "booting\nclaude> analyzing repo\nclaude> done, what next?", alive: true}
	p := newTestPlugin(f)
	ctx := context.Background()

	ref, err := p.Spawn(ctx, source.SpawnSpec{Title: "Fix lint", Prompt: "run the linter"})
	if err != nil {
		t.Fatal(err)
	}
	if ref.Source != "claude-code" || ref.ID != "s1" {
		t.Fatalf("spawn ref = %+v", ref)
	}
	// new-session started + the prompt was typed.
	if !f.sawArg("new-session") || !f.sawArg("run the linter") || !f.sawArg("Enter") {
		t.Fatalf("spawn did not drive tmux: %v", f.calls)
	}

	refs, err := p.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].ID != "s1" {
		t.Fatalf("list = %+v", refs)
	}

	st, _ := p.State(ctx, "s1")
	if st.Status != source.StatusRunning || !strings.Contains(st.Summary, "what next?") {
		t.Fatalf("state = %+v", st)
	}

	if err := p.Post(ctx, "s1", "yes, proceed"); err != nil {
		t.Fatal(err)
	}
	if !f.sawArg("yes, proceed") {
		t.Errorf("post did not send the message: %v", f.calls)
	}
}

// TestList_DropsDeadSession — a session tmux no longer has is pruned.
func TestList_DropsDeadSession(t *testing.T) {
	f := &fakeTmux{alive: false}
	p := newTestPlugin(f)
	ctx := context.Background()
	if _, err := p.Spawn(ctx, source.SpawnSpec{Title: "x"}); err != nil {
		t.Fatal(err)
	}
	refs, _ := p.List(ctx)
	if len(refs) != 0 {
		t.Fatalf("dead session not pruned: %+v", refs)
	}
}
