package service

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/iodesystems/yscr/config"
	"github.com/iodesystems/yscr/cue"
	"github.com/iodesystems/yscr/source"
	"github.com/iodesystems/yscr/store"
)

// cueRunner is phase 3 of the task cueing system: the release loop. Each fleet
// tick it runs the deterministic gate (cue.Plan) over the durable cue and, for
// every RELEASE decision, dispatches the task to its source (Post into an
// existing session, or Spawn a new one) and marks it in-flight.
//
// Autonomous, so it runs behind rails: it only exists when Cue.Enabled (the
// kill-switch); DryRun logs intended dispatches without acting (default on, so a
// freshly enabled cue never acts by accident); MaxPerHour hard-caps live
// dispatches; the cue.Caps bound concurrency. Driven from the watcher goroutine
// only — no internal locking.
//
// KNOWN LIMITATION (phase 4): dispatched tasks stay in-flight — completion
// detection (link task→session, MarkDone when the session reaches Done) isn't
// wired yet, so under live mode a session's per-session cap fills after its first
// dispatch. Safe under the default dry-run; required before sustained live use.
// cueStore is the slice of the durable cue the release loop needs (*store.PG
// satisfies it; a fake backs the unit test).
type cueStore interface {
	PendingTasks(ctx context.Context) ([]cue.Task, error)
	InflightTasks(ctx context.Context) ([]cue.Task, error)
	MarkInflight(ctx context.Context, id string, releasedAt int64) (bool, error)
}

type cueRunner struct {
	store      cueStore
	sources    map[string]source.Source
	caps       cue.Caps
	dryRun     bool
	maxPerHour int
	notify     func(title, body string)

	recent []int64 // ns timestamps of live dispatches, pruned to the last hour
}

// newCueRunner builds the runner, or returns nil when the cue is disabled or
// there's no durable store (the cue needs Postgres for the queue).
func newCueRunner(cfg config.CueConfig, pg *store.PG, sources []source.Source, notify func(string, string)) *cueRunner {
	if !cfg.Enabled || pg == nil {
		return nil
	}
	byID := make(map[string]source.Source, len(sources))
	for _, s := range sources {
		byID[s.ID()] = s
	}
	mode := "LIVE"
	if cfg.DryRunEnabled() {
		mode = "dry-run"
	}
	log.Printf("cue: release loop ENABLED (%s) — caps{perSession:%d global:%d spawns:%d} maxPerHour:%d",
		mode, cfg.PerSessionCap, cfg.GlobalCap, cfg.MaxSpawns, cfg.MaxPerHour)
	return &cueRunner{
		store:      pg,
		sources:    byID,
		caps:       cue.Caps{PerSession: cfg.PerSessionCap, Global: cfg.GlobalCap, MaxSpawns: cfg.MaxSpawns},
		dryRun:     cfg.DryRunEnabled(),
		maxPerHour: cfg.MaxPerHour,
		notify:     notify,
	}
}

// release runs one gate+dispatch pass over the current fleet snapshot.
func (r *cueRunner) release(ctx context.Context, states []source.State) {
	pending, err := r.store.PendingTasks(ctx)
	if err != nil {
		log.Printf("cue: pending: %v", err)
		return
	}
	if len(pending) == 0 {
		return
	}
	inflight, err := r.store.InflightTasks(ctx)
	if err != nil {
		log.Printf("cue: inflight: %v", err)
		return
	}

	r.pruneWindow()
	for _, d := range cue.Plan(pending, states, cue.Counts(inflight), r.caps, nil) {
		if !d.Release {
			continue
		}
		if r.maxPerHour > 0 && len(r.recent) >= r.maxPerHour {
			log.Printf("cue: hourly cap %d reached — holding the rest until the window rolls", r.maxPerHour)
			break
		}
		if r.dryRun {
			log.Printf("cue[dry-run]: would dispatch %s → %s: %q", d.Task.ID, d.Task.Target.Key(), truncate(d.Task.Prompt, 80))
			continue
		}
		if err := r.dispatch(ctx, d.Task); err != nil {
			log.Printf("cue: dispatch %s failed: %v (stays pending, retries next tick)", d.Task.ID, err)
			continue
		}
		if ok, err := r.store.MarkInflight(ctx, d.Task.ID, time.Now().UnixNano()); err != nil || !ok {
			// Dispatched but couldn't record it — log loudly; the guard on Post/Spawn
			// side effects is the source's problem, but avoid silent double-dispatch.
			log.Printf("cue: WARNING dispatched %s but MarkInflight ok=%v err=%v", d.Task.ID, ok, err)
		}
		r.recent = append(r.recent, time.Now().UnixNano())
		r.notify("Task dispatched", taskLabel(d.Task))
	}
}

// dispatch routes a released task: Spawn a new session or Post into an existing one.
func (r *cueRunner) dispatch(ctx context.Context, t cue.Task) error {
	src := r.sources[t.Target.Source]
	if src == nil {
		return fmt.Errorf("unknown source %q", t.Target.Source)
	}
	if t.Target.Spawn {
		sp, ok := src.(source.Spawner)
		if !ok {
			return fmt.Errorf("source %q is not a Spawner", t.Target.Source)
		}
		_, err := sp.Spawn(ctx, source.SpawnSpec{Prompt: t.Prompt, Dir: t.Target.SpawnDir})
		return err
	}
	return src.Post(ctx, t.Target.SessionID, t.Prompt)
}

// pruneWindow drops dispatch timestamps older than an hour (the MaxPerHour window).
func (r *cueRunner) pruneWindow() {
	if len(r.recent) == 0 {
		return
	}
	cutoff := time.Now().Add(-time.Hour).UnixNano()
	keep := r.recent[:0]
	for _, ts := range r.recent {
		if ts >= cutoff {
			keep = append(keep, ts)
		}
	}
	r.recent = keep
}

func taskLabel(t cue.Task) string {
	if t.Target.Spawn {
		return "spawn " + t.Target.Source + ": " + truncate(t.Prompt, 60)
	}
	return t.Target.Source + "/" + t.Target.SessionID + ": " + truncate(t.Prompt, 60)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
