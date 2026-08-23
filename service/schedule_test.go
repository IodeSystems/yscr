package service

import (
	"context"
	"testing"
	"time"

	"github.com/iodesystems/yscr/scratchpad"
)

func TestScheduler_PromotesDueOneShot(t *testing.T) {
	ctx := context.Background()
	pad := scratchpad.NewMem()
	now := time.Now()
	pad.Add(ctx, scratchpad.Task{ID: "t1", Prompt: "water plants", Kind: scratchpad.KindTodo,
		RunAt: now.Add(-time.Minute).UnixNano(), CreatedAt: 1})

	enq := &fakeEnq{}
	var notified []string
	s := newScheduler(pad, enq, func(_, body string) { notified = append(notified, body) })

	s.tick(ctx)

	if len(enq.tasks) != 1 || enq.tasks[0].ID != "t1" || enq.tasks[0].Prompt != "water plants" {
		t.Fatalf("promoted: %+v", enq.tasks)
	}
	// The scratchpad row is completed (no re-promote).
	got, _ := pad.Get(ctx, "t1")
	if got == nil || got.Status != scratchpad.StatusCompleted {
		t.Fatalf("row status: %+v", got)
	}
	if len(notified) != 1 {
		t.Fatalf("notify: %v", notified)
	}

	// Second tick: nothing to do (row closed, RunAt still set but not pending).
	s.tick(ctx)
	if len(enq.tasks) != 1 {
		t.Fatalf("re-promoted: %+v", enq.tasks)
	}
}

func TestScheduler_FutureRunAtHeld(t *testing.T) {
	ctx := context.Background()
	pad := scratchpad.NewMem()
	pad.Add(ctx, scratchpad.Task{ID: "t1", Prompt: "later", Kind: scratchpad.KindTodo,
		RunAt: time.Now().Add(time.Hour).UnixNano(), CreatedAt: 1})

	enq := &fakeEnq{}
	s := newScheduler(pad, enq, nil)
	s.tick(ctx)
	if len(enq.tasks) != 0 {
		t.Fatalf("future task promoted early: %+v", enq.tasks)
	}
}

func TestScheduler_CronRearm(t *testing.T) {
	ctx := context.Background()
	pad := scratchpad.NewMem()
	// A completed cron task → re-arm creates a pending successor.
	done, _ := pad.Add(ctx, scratchpad.Task{ID: "c1", Prompt: "daily report", Kind: scratchpad.KindTodo,
		Cron: "0 9 * * *", CreatedAt: 1})
	pad.Complete(ctx, done.ID, true)

	enq := &fakeEnq{}
	var notified []string
	s := newScheduler(pad, enq, func(_, body string) { notified = append(notified, body) })
	s.tick(ctx)

	// No promotion (cron tasks don't promote), but a successor exists.
	if len(enq.tasks) != 0 {
		t.Fatalf("cron task promoted: %+v", enq.tasks)
	}
	all, _ := pad.List(ctx)
	var succ *scratchpad.Task
	for i := range all {
		if all[i].ID != "c1" && all[i].Status == scratchpad.StatusPending && all[i].Cron == "0 9 * * *" {
			succ = &all[i]
		}
	}
	if succ == nil {
		t.Fatalf("no successor: %+v", all)
	}
	if succ.RunAt <= time.Now().UnixNano() {
		t.Fatalf("successor not in the future: %d", succ.RunAt)
	}
	if len(notified) != 1 {
		t.Fatalf("notify: %v", notified)
	}

	// Second tick: the successor is pending (not closed) → no second re-arm.
	s.tick(ctx)
	all, _ = pad.List(ctx)
	n := 0
	for _, t := range all {
		if t.Cron == "0 9 * * *" && t.Status == scratchpad.StatusPending {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("successor count: %d", n)
	}
}

func TestScheduler_BadCronLoggedNotFatal(t *testing.T) {
	ctx := context.Background()
	pad := scratchpad.NewMem()
	done, _ := pad.Add(ctx, scratchpad.Task{ID: "b1", Prompt: "broken", Kind: scratchpad.KindTodo,
		Cron: "not a cron", CreatedAt: 1})
	pad.Complete(ctx, done.ID, true)

	s := newScheduler(pad, &fakeEnq{}, nil)
	s.tick(ctx) // must not panic

	all, _ := pad.List(ctx)
	if len(all) != 1 {
		t.Fatalf("bad cron spawned a successor: %+v", all)
	}
}

func TestNewScheduler_NilGates(t *testing.T) {
	if newScheduler(nil, &fakeEnq{}, nil) != nil {
		t.Fatal("nil pad should give nil scheduler")
	}
	if newScheduler(scratchpad.NewMem(), nil, nil) != nil {
		t.Fatal("nil enqueuer should give nil scheduler")
	}
}
