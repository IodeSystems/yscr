package store

import (
	"context"
	"os"
	"testing"

	"github.com/iodesystems/yscr/scratchpad"
)

// testPGPad is like testPG but cleans the scratchpad table too.
func testPGPad(t *testing.T) (*PG, context.Context) {
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
	clean := func() { _, _ = pg.pool.Exec(ctx, `DELETE FROM scratchpad_tasks WHERE id LIKE 'padtest-%'`) }
	clean()
	t.Cleanup(func() { clean(); pg.Close() })
	return pg, ctx
}

func padTask(id, prompt string) scratchpad.Task {
	return scratchpad.Task{ID: "padtest-" + id, Prompt: prompt, Kind: scratchpad.KindTodo, CreatedAt: 100}
}

func TestScratchpadPG_AddDedupeLifecycle(t *testing.T) {
	pg, ctx := testPGPad(t)

	first, err := pg.Add(ctx, padTask("a", "buy milk"))
	if err != nil || first == nil {
		t.Fatalf("add: %v %v", first, err)
	}
	// Same prompt → derived key collision → no-op.
	dup, _ := pg.Add(ctx, padTask("b", "buy milk"))
	if dup != nil {
		t.Fatalf("dup add should be a no-op, got %+v", dup)
	}
	// Empty prompt is rejected cleanly.
	empty, err := pg.Add(ctx, scratchpad.Task{ID: "c", Prompt: "   ", Kind: scratchpad.KindTodo})
	if err != nil || empty != nil {
		t.Fatalf("empty add: %v %v", empty, err)
	}

	got, _ := pg.Get(ctx, "padtest-a")
	if got == nil || got.Prompt != "buy milk" || got.Kind != scratchpad.KindTodo {
		t.Fatalf("get: %+v", got)
	}

	// Complete is status-guarded.
	ok, _ := pg.Complete(ctx, "padtest-a", true)
	if !ok {
		t.Fatal("first complete should land")
	}
	if ok, _ := pg.Complete(ctx, "padtest-a", true); ok {
		t.Fatal("double complete should be a no-op")
	}
	if ok, _ := pg.Complete(ctx, "nope", true); ok {
		t.Fatal("unknown id should not land")
	}

	// Notes append.
	if ok, _ := pg.Note(ctx, "padtest-a", "picked up"); !ok {
		t.Fatal("note should land")
	}
	if ok, _ := pg.Note(ctx, "nope", "n"); ok {
		t.Fatal("note on unknown id should not land")
	}

	// A closed task's dedupe key frees up: re-adding the same prompt lands.
	readd, _ := pg.Add(ctx, padTask("d", "buy milk"))
	if readd == nil {
		t.Fatal("re-add after complete should land (key freed)")
	}
}

func TestScratchpadPG_ListOrderingAndFilter(t *testing.T) {
	pg, ctx := testPGPad(t)
	pg.Add(ctx, scratchpad.Task{ID: "padtest-p1", Prompt: "a", Kind: scratchpad.KindTodo, CreatedAt: 1})
	pg.Add(ctx, scratchpad.Task{ID: "padtest-p2", Prompt: "b", Kind: scratchpad.KindTodo, CreatedAt: 2})
	pg.Add(ctx, scratchpad.Task{ID: "padtest-c1", Prompt: "run ls", Kind: scratchpad.KindCommand, CreatedAt: 3})
	pg.Complete(ctx, "padtest-p1", true)

	all, err := pg.List(ctx)
	if err != nil || len(all) != 3 {
		t.Fatalf("list: %d %v", len(all), err)
	}
	// pending band newest-first: c1(3), p2(2); then closed: p1.
	if all[0].ID != "padtest-c1" || all[1].ID != "padtest-p2" || all[2].ID != "padtest-p1" {
		t.Fatalf("order: %+v", all)
	}

	cmds, _ := pg.List(ctx, scratchpad.KindCommand)
	if len(cmds) != 1 || cmds[0].ID != "padtest-c1" {
		t.Fatalf("filter: %+v", cmds)
	}
}

func TestScratchpadPG_SchedulingFields(t *testing.T) {
	pg, ctx := testPGPad(t)
	tt := scratchpad.Task{ID: "padtest-s1", Prompt: "water plants", Kind: scratchpad.KindTodo,
		RunAt: 999, Cron: "0 8 * * *", CreatedAt: 5}
	if _, err := pg.Add(ctx, tt); err != nil {
		t.Fatal(err)
	}
	got, _ := pg.Get(ctx, "padtest-s1")
	if got == nil || got.RunAt != 999 || got.Cron != "0 8 * * *" {
		t.Fatalf("round-trip: %+v", got)
	}
}
