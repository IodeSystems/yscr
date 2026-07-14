package service

import (
	"context"
	"testing"
	"time"

	"github.com/iodesystems/agentkit/llm"

	"github.com/iodesystems/yscr/cue"
	"github.com/iodesystems/yscr/source"
)

// fixedRunner returns a canned completion in one chunk.
type fixedRunner struct{ text string }

func (r fixedRunner) ChatStream(_ context.Context, _ []llm.Message, _ []llm.ToolDef, _ *llm.ChatOpts) (<-chan llm.StreamChunk, error) {
	ch := make(chan llm.StreamChunk, 2)
	ch <- llm.StreamChunk{Content: r.text}
	ch <- llm.StreamChunk{Done: true}
	close(ch)
	return ch, nil
}

type fakeEnq struct {
	tasks  []cue.Task
	reject map[string]bool // dedupe_key → EnqueueTask returns false (already live)
}

func (f *fakeEnq) EnqueueTask(_ context.Context, t cue.Task, _ int64) (bool, error) {
	f.tasks = append(f.tasks, t)
	return !f.reject[t.DedupeKey], nil
}

func gen(text string, enq cueEnqueuer) *cueGenerator {
	return &cueGenerator{
		runner:   fixedRunner{text: text},
		enq:      enq,
		fleet:    func(context.Context) []source.State { return nil },
		goals:    []string{"ship it"},
		interval: time.Hour,
	}
}

func TestGenerate_ParsesEnqueuesSkipsMalformed(t *testing.T) {
	// Wrapped in prose + a code fence; a third task is malformed (no prompt).
	out := "Sure:\n```json\n" + `{"tasks":[
	  {"prompt":"write tests","source":"cc","session_id":"s1","priority":2,"dedupe_key":"k1"},
	  {"prompt":"start docs","source":"cc","spawn":true,"dir":"/docs","dedupe_key":"k2"},
	  {"prompt":"","source":"cc"}
	]}` + "\n```\n"
	fe := &fakeEnq{}
	n, err := gen(out, fe).generateOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("enqueued %d, want 2 (third is malformed)", n)
	}
	if len(fe.tasks) != 2 {
		t.Fatalf("malformed task should be skipped before enqueue: %+v", fe.tasks)
	}
	a := fe.tasks[0]
	if a.Prompt != "write tests" || a.Target.Source != "cc" || a.Target.SessionID != "s1" || a.Priority != 2 || a.DedupeKey != "k1" {
		t.Errorf("task 0 wrong: %+v", a)
	}
	if a.ID == "" {
		t.Error("task must get a generated id")
	}
	b := fe.tasks[1]
	if !b.Target.Spawn || b.Target.SpawnDir != "/docs" || b.DedupeKey != "k2" {
		t.Errorf("task 1 (spawn) wrong: %+v", b)
	}
}

func TestGenerate_DedupeSkipCountsOnlyNew(t *testing.T) {
	out := `{"tasks":[
	  {"prompt":"a","source":"cc","dedupe_key":"k1"},
	  {"prompt":"b","source":"cc","dedupe_key":"k2"}
	]}`
	fe := &fakeEnq{reject: map[string]bool{"k1": true}} // k1 already live
	n, _ := gen(out, fe).generateOnce(context.Background())
	if n != 1 {
		t.Fatalf("only the new task counts: got %d, want 1", n)
	}
}

func TestGenerate_DerivesDedupeKey(t *testing.T) {
	out := `{"tasks":[{"prompt":"do it","source":"cc","session_id":"s1"}]}`
	fe := &fakeEnq{}
	if _, err := gen(out, fe).generateOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := "cc/s1|do it"
	if fe.tasks[0].DedupeKey != want {
		t.Errorf("derived dedupe key = %q, want %q", fe.tasks[0].DedupeKey, want)
	}
}

func TestGenerate_EmptyTasks(t *testing.T) {
	fe := &fakeEnq{}
	n, err := gen(`{"tasks":[]}`, fe).generateOnce(context.Background())
	if err != nil || n != 0 {
		t.Fatalf("empty proposal: n=%d err=%v", n, err)
	}
}

func TestNewCueGenerator_NilWithoutGoals(t *testing.T) {
	if g := newCueGenerator(configCueGen{Goals: nil, GenInterval: 120}, fixedRunner{}, &fakeEnq{}, nil); g != nil {
		t.Error("no goals → nil generator")
	}
	if g := newCueGenerator(configCueGen{Goals: []string{"g"}, GenInterval: 120}, fixedRunner{}, &fakeEnq{}, func(context.Context) []source.State { return nil }); g == nil {
		t.Error("goals + runner + enq → generator")
	}
}
