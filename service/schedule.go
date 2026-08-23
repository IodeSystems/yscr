// schedule.go is the scratchpad scheduler tick: it turns scheduled todos into
// releasable work on the existing cue pipeline. Two jobs, run from the fleet
// watcher (12s cadence — minute-resolution cron doesn't need faster):
//
//  1. re-arm: a closed task with a cron spec gets a fresh pending successor at
//     its next occurrence (the original row stays as history). The successor
//     shares the DedupeKey, so while it's live no duplicate can be added.
//  2. release: a pending task whose RunAt has passed is promoted into the cue
//     queue (EnqueueTask) so the deterministic release gate + dispatch handle
//     it from there — the scratchpad never dispatches directly.
package service

import (
	"context"
	"log"
	"time"

	"github.com/google/uuid"

	"github.com/iodesystems/yscr/cue"
	"github.com/iodesystems/yscr/scratchpad"
)

type scheduler struct {
	pad    scratchpad.Store
	enq    cueEnqueuer
	notify func(title, body string)
}

// newScheduler builds the tick, or nil when there's no durable store (the
// scratchpad needs Postgres to survive restart).
func newScheduler(pad scratchpad.Store, enq cueEnqueuer, notify func(string, string)) *scheduler {
	if pad == nil || enq == nil {
		return nil
	}
	return &scheduler{pad: pad, enq: enq, notify: notify}
}

// tick runs one scheduler pass. Errors are logged, not returned — a bad row
// must never jam the fleet watcher.
func (s *scheduler) tick(ctx context.Context) {
	now := time.Now()
	tasks, err := s.pad.List(ctx)
	if err != nil {
		log.Printf("schedule: list: %v", err)
		return
	}
	for _, t := range tasks {
		switch {
		case t.Cron != "" && (t.Status == scratchpad.StatusDone || t.Status == scratchpad.StatusFailed || t.Status == scratchpad.StatusCompleted):
			s.rearm(ctx, t, now)
		case t.RunAt > 0 && t.Status == scratchpad.StatusPending && now.UnixNano() >= t.RunAt:
			s.promote(ctx, t, now)
		}
	}
}

// rearm creates the next-occurrence successor of a closed cron task.
func (s *scheduler) rearm(ctx context.Context, t scratchpad.Task, now time.Time) {
	next, err := scratchpad.NextRun(t.Cron, now)
	if err != nil {
		log.Printf("schedule: bad cron %q on %s: %v", t.Cron, t.ID, err)
		return
	}
	if next == 0 {
		return // no occurrence within the horizon; leave it closed
	}
	succ := scratchpad.Task{
		ID:        uuid.NewString(),
		DedupeKey: t.DedupeKey, // same identity → blocks a duplicate while live
		Prompt:    t.Prompt,
		Kind:      t.Kind,
		Priority:  t.Priority,
		RunAt:     next,
		Cron:      t.Cron,
		Target:    t.Target,
		CreatedAt: now.UnixNano(),
	}
	if _, err := s.pad.Add(ctx, succ); err != nil {
		log.Printf("schedule: re-arm %s: %v", t.ID, err)
		return
	}
	log.Printf("schedule: re-armed %q (cron %s)", t.Prompt, t.Cron)
	if s.notify != nil {
		s.notify("Scheduled task re-armed", t.Prompt)
	}
}

// promote hands a due one-shot task to the cue pipeline. The scratchpad row is
// completed first: if EnqueueTask then fails, the task shows as done (visible,
// not silently lost) rather than re-promoting every tick; the shared DedupeKey
// makes a manual retry safe.
func (s *scheduler) promote(ctx context.Context, t scratchpad.Task, now time.Time) {
	ok, err := s.pad.Complete(ctx, t.ID, true)
	if err != nil || !ok {
		return // already closed by someone else — don't double-promote
	}
	ct := cue.Task{
		ID:        t.ID, // same id: the cue row IS this task's continuation
		DedupeKey: t.DedupeKey,
		Prompt:    t.Prompt,
		Priority:  t.Priority,
		CreatedAt: now.UnixNano(),
		Target: cue.Target{
			Source:    t.Target.Source,
			SessionID: t.Target.SessionID,
			Spawn:     t.Target.Spawn,
			SpawnDir:  t.Target.SpawnDir,
		},
	}
	if _, err := s.enq.EnqueueTask(ctx, ct, now.UnixNano()); err != nil {
		log.Printf("schedule: promote %s: %v", t.ID, err)
		return
	}
	if !ok {
		log.Printf("schedule: promote %s: dedupe no-op (already cued)", t.ID)
		return
	}
	log.Printf("schedule: promoted %q to the cue", t.Prompt)
	if s.notify != nil {
		s.notify("Scheduled task released", t.Prompt)
	}
}
