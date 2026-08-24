package concierge

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/iodesystems/yscr/source"
	"github.com/iodesystems/yscr/store"
)

type fakeSpawner struct{}

func (f *fakeSpawner) ID() string                                        { return "terminal" }
func (f *fakeSpawner) List(context.Context) ([]source.SessionRef, error) { return nil, nil }
func (f *fakeSpawner) State(context.Context, string) (source.State, error) {
	return source.State{}, nil
}
func (f *fakeSpawner) Observe(context.Context, string) (<-chan source.Event, error) {
	return nil, nil
}
func (f *fakeSpawner) Post(context.Context, string, string) error { return nil }

func (f *fakeSpawner) Spawn(ctx context.Context, spec source.SpawnSpec) (source.SessionRef, error) {
	if spec.Prompt == "boom" {
		return source.SessionRef{}, errors.New("launch failed")
	}
	return source.SessionRef{Source: "terminal", ID: "p1", Title: firstWord(spec.Prompt), Dir: spec.Dir}, nil
}

func TestRunCommand_Foreground(t *testing.T) {
	c := New(nil, store.NewMem(), &fakeSource{}).WithRun(&fakeSpawner{}, func(ctx context.Context, ref source.SessionRef) (string, error) {
		if ref.ID != "p1" {
			t.Errorf("wait got ref %+v", ref)
		}
		return "ok: 3 passed", nil
	})
	out := c.runCommand(context.Background(), map[string]any{"command": "go test ./..."})
	if !strings.Contains(out, "finished") || !strings.Contains(out, "ok: 3 passed") {
		t.Errorf("out = %q", out)
	}
}

func TestRunCommand_Background(t *testing.T) {
	c := New(nil, store.NewMem(), &fakeSource{}).WithRun(&fakeSpawner{}, func(context.Context, source.SessionRef) (string, error) {
		t.Fatal("wait must not be called in background mode")
		return "", nil
	})
	out := c.runCommand(context.Background(), map[string]any{"command": "make build", "background": true})
	if !strings.Contains(out, "background") || !strings.Contains(out, "p1") {
		t.Errorf("out = %q", out)
	}
}

func TestRunCommand_Errors(t *testing.T) {
	c := New(nil, store.NewMem(), &fakeSource{}).WithRun(&fakeSpawner{}, func(context.Context, source.SessionRef) (string, error) { return "", nil })
	if out := c.runCommand(context.Background(), map[string]any{"command": "   "}); !strings.Contains(out, "needs a command") {
		t.Errorf("out = %q", out)
	}
	out := c.runCommand(context.Background(), map[string]any{"command": "boom"})
	if !strings.Contains(out, "launch failed") {
		t.Errorf("out = %q", out)
	}
}

func TestRunCommand_OffWithoutWithRun(t *testing.T) {
	c := New(nil, store.NewMem(), &fakeSource{})
	out := c.runCommand(context.Background(), map[string]any{"command": "ls"})
	if !strings.Contains(out, "unavailable") {
		t.Errorf("out = %q", out)
	}
}

func TestRunCommand_SummarizesLongOutput(t *testing.T) {
	long := strings.Repeat("ok pkg\n", 300) // > 1200 chars
	c := New(nil, store.NewMem(), &fakeSource{}).WithRun(&fakeSpawner{},
		func(context.Context, source.SessionRef) (string, error) { return long, nil },
		func(context.Context, string, string) (string, error) { return "all tests passed", nil })
	out := c.runCommand(context.Background(), map[string]any{"command": "go test ./..."})
	if !strings.Contains(out, "summary: all tests passed") || !strings.Contains(out, "ok pkg") {
		t.Fatalf("expected summary + tail, got: %.200s", out)
	}
}

func TestRunCommand_ShortOutputSkipsSummarizer(t *testing.T) {
	called := false
	c := New(nil, store.NewMem(), &fakeSource{}).WithRun(&fakeSpawner{},
		func(context.Context, source.SessionRef) (string, error) { return "hi\n", nil },
		func(context.Context, string, string) (string, error) { called = true; return "x", nil })
	out := c.runCommand(context.Background(), map[string]any{"command": "echo hi"})
	if called {
		t.Fatal("summarizer should not run for short output")
	}
	if !strings.Contains(out, "hi") {
		t.Fatalf("got %q", out)
	}
}

func TestRunCommand_SummarizerFailureFallsBack(t *testing.T) {
	long := strings.Repeat("line\n", 300)
	c := New(nil, store.NewMem(), &fakeSource{}).WithRun(&fakeSpawner{},
		func(context.Context, source.SessionRef) (string, error) { return long, nil },
		func(context.Context, string, string) (string, error) { return "", context.DeadlineExceeded })
	out := c.runCommand(context.Background(), map[string]any{"command": "x"})
	if !strings.Contains(out, "output tail:") || !strings.Contains(out, "line") {
		t.Fatalf("expected raw-tail fallback, got: %.200s", out)
	}
}
