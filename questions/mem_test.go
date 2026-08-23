package questions

import (
	"context"
	"testing"
)

func TestMem_DedupeAndLifecycle(t *testing.T) {
	ctx := context.Background()
	m := NewMem()

	q1, err := m.Add(ctx, Question{ID: "a", Question: "Which region?", CreatedAt: 1})
	if err != nil || q1 == nil {
		t.Fatalf("add: %v %v", q1, err)
	}
	// Same text (different case/whitespace) → dedupe no-op.
	dup, _ := m.Add(ctx, Question{ID: "b", Question: "  which   REGION? ", CreatedAt: 2})
	if dup != nil {
		t.Fatalf("dup should be a no-op: %+v", dup)
	}
	// Empty question rejected.
	empty, _ := m.Add(ctx, Question{ID: "c", Question: "   "})
	if empty != nil {
		t.Fatal("empty should be rejected")
	}

	qs, _ := m.List(ctx)
	if len(qs) != 1 || qs[0].Status != StatusOpen {
		t.Fatalf("list: %+v", qs)
	}

	ok, _ := m.Answer(ctx, "a", "EU")
	if !ok {
		t.Fatal("answer should land")
	}
	if ok, _ := m.Answer(ctx, "a", "US"); ok {
		t.Fatal("double answer should be a no-op")
	}
	qs, _ = m.List(ctx)
	if len(qs) != 1 || qs[0].Status != StatusAnswered || qs[0].Answer != "EU" {
		t.Fatalf("after answer: %+v", qs)
	}
	// Answered frees the dedupe key.
	readd, _ := m.Add(ctx, Question{ID: "d", Question: "Which region?", CreatedAt: 3})
	if readd == nil {
		t.Fatal("re-add after answer should land")
	}
}

func TestMem_ListOrdering(t *testing.T) {
	ctx := context.Background()
	m := NewMem()
	m.Add(ctx, Question{ID: "old", Question: "first?", CreatedAt: 1})
	m.Add(ctx, Question{ID: "new", Question: "second?", CreatedAt: 2})
	qs, _ := m.List(ctx)
	if len(qs) != 2 || qs[0].ID != "old" || qs[1].ID != "new" {
		t.Fatalf("open oldest-first: %+v", qs)
	}
	m.Answer(ctx, "old", "yes")
	qs, _ = m.List(ctx)
	if qs[0].ID != "new" || qs[1].ID != "old" {
		t.Fatalf("open before answered: %+v", qs)
	}
}
