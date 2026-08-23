package scratchpad

import (
	"context"
	"testing"
)

func TestAddDedupe(t *testing.T) {
	ctx := context.Background()
	m := NewMem()
	first, err := m.Add(ctx, Task{ID: "1", Prompt: "buy milk", Kind: KindTodo})
	if err != nil || first == nil {
		t.Fatalf("add: %v %v", first, err)
	}
	// Same prompt (derived key) → dedupe no-op.
	dup, _ := m.Add(ctx, Task{ID: "2", Prompt: "buy milk", Kind: KindTodo})
	if dup != nil {
		t.Fatalf("dup add should be a no-op, got %+v", dup)
	}
	// Different kind → different derived key → allowed.
	other, _ := m.Add(ctx, Task{ID: "3", Prompt: "buy milk", Kind: KindCommand})
	if other == nil {
		t.Fatal("different-kind add should land")
	}
}

func TestListOrdering(t *testing.T) {
	ctx := context.Background()
	m := NewMem()
	m.Add(ctx, Task{ID: "p1", Prompt: "a", Kind: KindTodo, CreatedAt: 1})
	m.Add(ctx, Task{ID: "p2", Prompt: "b", Kind: KindTodo, CreatedAt: 2})
	m.Complete(ctx, "p1", true) // closed → last band
	all, _ := m.List(ctx)
	if len(all) != 2 {
		t.Fatalf("len %d", len(all))
	}
	// p2 (pending, newer) before p1 (completed).
	if all[0].ID != "p2" || all[1].ID != "p1" {
		t.Fatalf("order: %+v", all)
	}
	// Kind filter.
	cmds, _ := m.List(ctx, KindCommand)
	if len(cmds) != 0 {
		t.Fatalf("filter leaked: %+v", cmds)
	}
}

func TestCompleteGuards(t *testing.T) {
	ctx := context.Background()
	m := NewMem()
	m.Add(ctx, Task{ID: "1", Prompt: "x", Kind: KindTodo})
	if ok, _ := m.Complete(ctx, "1", true); !ok {
		t.Fatal("first complete should land")
	}
	if ok, _ := m.Complete(ctx, "1", true); ok {
		t.Fatal("double complete should be a no-op")
	}
	if ok, _ := m.Complete(ctx, "nope", true); ok {
		t.Fatal("unknown id should not land")
	}
}

func TestNoteAndGet(t *testing.T) {
	ctx := context.Background()
	m := NewMem()
	m.Add(ctx, Task{ID: "1", Prompt: "x", Kind: KindTodo})
	if ok, _ := m.Note(ctx, "1", "started"); !ok {
		t.Fatal("note should land")
	}
	if ok, _ := m.Note(ctx, "nope", "n"); ok {
		t.Fatal("note on unknown id should not land")
	}
	got, _ := m.Get(ctx, "1")
	if got == nil || got.Prompt != "x" {
		t.Fatalf("get: %+v", got)
	}
	if len(m.Notes("1")) != 1 {
		t.Fatal("note not stored")
	}
}

func TestNormalize(t *testing.T) {
	tt := Normalize(Task{Prompt: "  buy milk  ", Kind: KindTodo, Cron: " 0 9 * * * "})
	if tt.Prompt != "buy milk" || tt.Cron != "0 9 * * *" {
		t.Fatalf("normalize: %+v", tt)
	}
	if tt.DedupeKey != "todo|buy milk" {
		t.Fatalf("key: %q", tt.DedupeKey)
	}
}
