package concierge

import (
	"context"
	"strings"
	"testing"

	"github.com/iodesystems/yscr/scratchpad"
	"github.com/iodesystems/yscr/store"
)

func TestTaskDispatch_AddListDone(t *testing.T) {
	ctx := context.Background()
	c := New(nil, store.NewMem(), &fakeSource{}).WithTasks(scratchpad.NewMem())

	out := c.taskDispatch(ctx, "add_task", map[string]any{
		"prompt": "buy milk",
	})
	if !strings.Contains(out, "added task") {
		t.Fatalf("add: %q", out)
	}
	// id is the second token, quoted.
	id := out[len("added task "):]
	if i := strings.Index(id, ":"); i >= 0 {
		id = id[:i]
	}
	id = strings.Trim(id, `"`)

	// Duplicate → no-op message.
	out = c.taskDispatch(ctx, "add_task", map[string]any{"prompt": "buy milk"})
	if !strings.Contains(out, "duplicate") {
		t.Fatalf("dup: %q", out)
	}

	// Scheduled + targeted add.
	out = c.taskDispatch(ctx, "add_task", map[string]any{
		"prompt":   "run the build",
		"kind":     "command",
		"cron":     "0 9 * * *",
		"source":   "fake",
		"id":       "s1",
		"priority": float64(3),
	})
	if !strings.Contains(out, "added task") {
		t.Fatalf("scheduled add: %q", out)
	}

	out = c.taskDispatch(ctx, "list_tasks", map[string]any{})
	if !strings.Contains(out, "buy milk") || !strings.Contains(out, "run the build") ||
		!strings.Contains(out, "cron 0 9 * * *") || !strings.Contains(out, "fake/s1") {
		t.Fatalf("list: %q", out)
	}

	out = c.taskDispatch(ctx, "done_task", map[string]any{"id": id})
	if out != "marked done." {
		t.Fatalf("done: %q", out)
	}
	// Double-done → no-op.
	out = c.taskDispatch(ctx, "done_task", map[string]any{"id": id})
	if !strings.Contains(out, "no open task") {
		t.Fatalf("double done: %q", out)
	}
}

func TestTaskDispatch_Validation(t *testing.T) {
	ctx := context.Background()
	c := New(nil, store.NewMem(), &fakeSource{}).WithTasks(scratchpad.NewMem())

	if out := c.taskDispatch(ctx, "add_task", map[string]any{"prompt": "x", "source": "nope"}); !strings.Contains(out, "no source") {
		t.Fatalf("unknown source: %q", out)
	}
	if out := c.taskDispatch(ctx, "add_task", map[string]any{"prompt": "x", "source": "fake", "id": "s1"}); !strings.Contains(out, "added task") {
		t.Fatalf("targeted add: %q", out)
	}
	if out := c.taskDispatch(ctx, "add_task", map[string]any{"prompt": "spawn job", "source": "fake", "spawn": true, "dir": "/tmp/w"}); !strings.Contains(out, "added task") {
		t.Fatalf("spawn with dir: %q", out)
	}
	if out := c.taskDispatch(ctx, "done_task", map[string]any{"id": "ghost"}); !strings.Contains(out, "no open task") {
		t.Fatalf("ghost done: %q", out)
	}
}

func TestWithTasks_NilStoreLeavesToolsOff(t *testing.T) {
	c := New(nil, store.NewMem(), &fakeSource{})
	if c.taskToolsOn || c.tasks != nil {
		t.Fatal("default should have no task tools")
	}
	out := c.taskDispatch(context.Background(), "list_tasks", map[string]any{})
	if !strings.Contains(out, "not configured") {
		t.Fatalf("nil store: %q", out)
	}
}
