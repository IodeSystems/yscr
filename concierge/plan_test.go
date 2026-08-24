package concierge

import (
	"context"
	"strings"
	"testing"

	"github.com/iodesystems/yscr/cue"
)

// fakeEnq records enqueued tasks for plan_goal tests.
type fakeEnq struct {
	tasks []cue.Task
	dup   map[string]bool // ids that should report "already live"
}

func (f *fakeEnq) EnqueueTask(ctx context.Context, t cue.Task, created int64) (bool, error) {
	if f.dup[t.ID] {
		return false, nil
	}
	f.tasks = append(f.tasks, t)
	return true, nil
}

func planArgs(goal string, tasks ...map[string]any) map[string]any {
	raw := make([]any, 0, len(tasks))
	for _, t := range tasks {
		raw = append(raw, t)
	}
	return map[string]any{"goal": goal, "tasks": raw}
}

func TestPlanGoal_BasicBatch(t *testing.T) {
	enq := &fakeEnq{}
	c := New(nil, nil).WithPlanGoal(enq)
	out := c.planDispatch(context.Background(), planArgs(
		"ship the thing",
		map[string]any{"id": "a", "prompt": "do A", "source": "claude-code"},
		map[string]any{"id": "b", "prompt": "do B", "source": "claude-code", "deps": []any{"a"}},
	))
	if len(enq.tasks) != 2 {
		t.Fatalf("expected 2 enqueued, got %d: %s", len(enq.tasks), out)
	}
	if len(enq.tasks[1].Deps) != 1 || enq.tasks[1].Deps[0] != "a" {
		t.Errorf("deps not carried: %+v", enq.tasks[1].Deps)
	}
	if !strings.Contains(out, "2 task(s)") {
		t.Errorf("report should say 2 tasks: %s", out)
	}
}

func TestPlanGoal_UnknownDepRelaxed(t *testing.T) {
	enq := &fakeEnq{}
	c := New(nil, nil).WithPlanGoal(enq)
	out := c.planDispatch(context.Background(), planArgs(
		"g",
		map[string]any{"id": "a", "prompt": "do A", "source": "claude-code"},
		map[string]any{"id": "b", "prompt": "do B", "source": "claude-code", "deps": []any{"ghost"}},
	))
	if len(enq.tasks) != 2 {
		t.Fatalf("both tasks must still enqueue, got %d: %s", len(enq.tasks), out)
	}
	for _, task := range enq.tasks {
		if task.ID == "b" && len(task.Deps) != 0 {
			t.Errorf("unknown dep should be dropped: %+v", task.Deps)
		}
	}
	if !strings.Contains(out, "relaxed") {
		t.Errorf("report should mention relaxed deps: %s", out)
	}
}

func TestPlanGoal_CycleRelaxed(t *testing.T) {
	enq := &fakeEnq{}
	c := New(nil, nil).WithPlanGoal(enq)
	out := c.planDispatch(context.Background(), planArgs(
		"g",
		map[string]any{"id": "a", "prompt": "do A", "source": "claude-code", "deps": []any{"b"}},
		map[string]any{"id": "b", "prompt": "do B", "source": "claude-code", "deps": []any{"a"}},
	))
	if len(enq.tasks) != 2 {
		t.Fatalf("both tasks must enqueue, got %d: %s", len(enq.tasks), out)
	}
	// The cycle must be broken: at least one task has no deps.
	broken := false
	for _, t := range enq.tasks {
		if len(t.Deps) == 0 {
			broken = true
		}
	}
	if !broken {
		t.Errorf("cycle should have been relaxed: %+v", enq.tasks)
	}
}

func TestPlanGoal_MalformedSkipped(t *testing.T) {
	enq := &fakeEnq{}
	c := New(nil, nil).WithPlanGoal(enq)
	out := c.planDispatch(context.Background(), planArgs(
		"g",
		map[string]any{"prompt": "no source"},
		map[string]any{"id": "ok", "source": "claude-code"}, // no prompt
		map[string]any{"id": "a", "prompt": "fine", "source": "claude-code"},
	))
	if len(enq.tasks) != 1 || enq.tasks[0].ID != "a" {
		t.Fatalf("only the valid task should enqueue: %+v (%s)", enq.tasks, out)
	}
}

func TestPlanGoal_Empty(t *testing.T) {
	enq := &fakeEnq{}
	c := New(nil, nil).WithPlanGoal(enq)
	if out := c.planDispatch(context.Background(), planArgs("", nil)); !strings.Contains(out, "needs a goal") {
		t.Errorf("empty plan should be rejected: %s", out)
	}
	if len(enq.tasks) != 0 {
		t.Errorf("nothing should enqueue: %+v", enq.tasks)
	}
}

func TestPlanGoal_Dedup(t *testing.T) {
	enq := &fakeEnq{dup: map[string]bool{"a": true}}
	c := New(nil, nil).WithPlanGoal(enq)
	out := c.planDispatch(context.Background(), planArgs(
		"g",
		map[string]any{"id": "a", "prompt": "do A", "source": "claude-code"},
	))
	if len(enq.tasks) != 0 {
		t.Fatalf("dup should not re-enqueue: %+v", enq.tasks)
	}
	if !strings.Contains(out, "already live") {
		t.Errorf("report should mention dedupe: %s", out)
	}
}

func TestPlanGoal_NoStore(t *testing.T) {
	c := New(nil, nil) // no WithPlanGoal
	out := c.planDispatch(context.Background(), planArgs("g", map[string]any{"id": "a", "prompt": "x", "source": "s"}))
	if !strings.Contains(out, "not configured") {
		t.Errorf("should report unconfigured: %s", out)
	}
}
