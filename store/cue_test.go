package store

import (
	"context"
	"os"
	"testing"

	"github.com/iodesystems/yscr/cue"
	"github.com/iodesystems/yscr/source"
)

// testPG connects to yscr-pg (or YSCR_TEST_DSN); skips if unreachable so the
// suite stays green without a database. Cleans up its own rows (id prefix).
func testPG(t *testing.T) (*PG, context.Context) {
	t.Helper()
	dsn := os.Getenv("YSCR_TEST_DSN")
	if dsn == "" {
		dsn = "postgres://yscr:yscr@127.0.0.1:8001/yscr?sslmode=disable"
	}
	ctx := context.Background()
	pg, err := NewPG(ctx, dsn)
	if err != nil {
		t.Skipf("no test db (%v)", err)
	}
	clean := func() { _, _ = pg.pool.Exec(ctx, `DELETE FROM cue_tasks WHERE id LIKE 'cuetest-%'`) }
	clean()
	t.Cleanup(func() { clean(); pg.Close() })
	return pg, ctx
}

func task(id, dedupe string, prio int, tgt cue.Target) cue.Task {
	return cue.Task{ID: "cuetest-" + id, DedupeKey: dedupe, Prompt: "do " + id, Priority: prio, Target: tgt}
}

func TestCueStore_EnqueueDedupeLifecycle(t *testing.T) {
	pg, ctx := testPG(t)

	tgt := cue.Target{Source: "cc", SessionID: "s1"}
	ok, err := pg.EnqueueTask(ctx, task("a", "dk1", 5, tgt), 100)
	if err != nil || !ok {
		t.Fatalf("first enqueue: ok=%v err=%v", ok, err)
	}
	// Same dedupe key while the first is live → skipped.
	ok, err = pg.EnqueueTask(ctx, task("b", "dk1", 5, tgt), 101)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected dedupe skip for a live DedupeKey")
	}
	// Empty dedupe key opts out of dedup → always inserts.
	if ok, err := pg.EnqueueTask(ctx, task("c", "", 1, cue.Target{Source: "cc", SessionID: "s2"}), 102); err != nil || !ok {
		t.Fatalf("empty-dedupe enqueue: ok=%v err=%v", ok, err)
	}
	// Re-enqueue same id → no-op.
	if ok, _ := pg.EnqueueTask(ctx, task("a", "dk1", 5, tgt), 103); ok {
		t.Fatal("re-enqueue of existing id should be a no-op")
	}

	pending, err := pg.PendingTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 { // a and c (b was deduped)
		t.Fatalf("pending=%d, want 2: %+v", len(pending), pending)
	}
	if pending[0].ID != "cuetest-a" { // priority 5 before 1
		t.Errorf("pending order: got %s first, want cuetest-a (higher priority)", pending[0].ID)
	}

	// Release "a": pending → inflight (guarded).
	if ok, err := pg.MarkInflight(ctx, "cuetest-a", 200); err != nil || !ok {
		t.Fatalf("MarkInflight: ok=%v err=%v", ok, err)
	}
	if ok, _ := pg.MarkInflight(ctx, "cuetest-a", 201); ok {
		t.Error("double release should be a no-op")
	}

	inflight, err := pg.InflightTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	counts := cue.Counts(inflight)
	if counts["cc/s1"] != 1 {
		t.Fatalf("inflight counts: got %v, want cc/s1=1", counts)
	}

	// Now that "a" is inflight (not pending), a same-dedupe re-propose is still blocked.
	if ok, _ := pg.EnqueueTask(ctx, task("d", "dk1", 5, tgt), 300); ok {
		t.Error("dedupe should block while the task is inflight, not just pending")
	}

	if ok, err := pg.MarkDone(ctx, "cuetest-a", 400); err != nil || !ok {
		t.Fatalf("MarkDone: ok=%v err=%v", ok, err)
	}
	// After done, the dedupe key is free again.
	if ok, err := pg.EnqueueTask(ctx, task("e", "dk1", 5, tgt), 500); err != nil || !ok {
		t.Fatalf("dedupe should free after done: ok=%v err=%v", ok, err)
	}
}

// TestCueStore_FeedsPlan drives the phase-1 gate off live store state.
func TestCueStore_FeedsPlan(t *testing.T) {
	pg, ctx := testPG(t)

	_, _ = pg.EnqueueTask(ctx, task("p1", "", 1, cue.Target{Source: "cc", SessionID: "idle"}), 1)
	_, _ = pg.EnqueueTask(ctx, task("p2", "", 1, cue.Target{Source: "cc", SessionID: "busy"}), 2)

	pending, _ := pg.PendingTasks(ctx)
	inflight, _ := pg.InflightTasks(ctx)

	decisions := cue.Plan(pending, []source.State(nil), cue.Counts(inflight), cue.Caps{}, nil)
	// No fleet → every existing-session task holds ("not in fleet"); proves the
	// store rows round-trip cleanly into Plan.
	for _, d := range decisions {
		if d.Release {
			t.Errorf("no fleet, task %s should hold, got release", d.Task.ID)
		}
	}
	if len(decisions) != 2 {
		t.Fatalf("expected 2 decisions from 2 pending, got %d", len(decisions))
	}
}
