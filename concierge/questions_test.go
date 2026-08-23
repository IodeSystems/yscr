package concierge

import (
	"context"
	"strings"
	"testing"

	"github.com/iodesystems/yscr/questions"
	"github.com/iodesystems/yscr/store"
)

func TestQuestionDispatch_ParkListAnswer(t *testing.T) {
	ctx := context.Background()
	c := New(nil, store.NewMem(), &fakeSource{}).WithQuestions(questions.NewMem())

	out := c.questionDispatch(ctx, "ask_question", map[string]any{
		"question": "which region should the deploy target?",
		"context":  "deploy task is blocked on it",
	})
	if !strings.Contains(out, "parked question") {
		t.Fatalf("park: %q", out)
	}
	id := out[len("parked question "):]
	if i := strings.Index(id, ":"); i >= 0 {
		id = id[:i]
	}

	// Duplicate text → no-op.
	out = c.questionDispatch(ctx, "ask_question", map[string]any{"question": "which region should the deploy target?"})
	if !strings.Contains(out, "already open") {
		t.Fatalf("dup: %q", out)
	}

	out = c.questionDispatch(ctx, "list_questions", map[string]any{})
	if !strings.Contains(out, "region") || !strings.Contains(out, id) {
		t.Fatalf("list: %q", out)
	}

	out = c.questionDispatch(ctx, "answer_question", map[string]any{"id": id, "answer": "EU"})
	if out != "recorded. Proceed with the work it was holding." {
		t.Fatalf("answer: %q", out)
	}
	// Double-answer → no-op.
	out = c.questionDispatch(ctx, "answer_question", map[string]any{"id": id, "answer": "US"})
	if !strings.Contains(out, "no open question") {
		t.Fatalf("double answer: %q", out)
	}
}

func TestQuestionDispatch_NilStore(t *testing.T) {
	c := New(nil, store.NewMem(), &fakeSource{})
	if c.questionToolsOn || c.questions != nil {
		t.Fatal("default should have no question tools")
	}
	out := c.questionDispatch(context.Background(), "list_questions", map[string]any{})
	if !strings.Contains(out, "not configured") {
		t.Fatalf("nil store: %q", out)
	}
}
