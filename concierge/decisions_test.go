package concierge

import (
	"context"
	"strings"
	"testing"

	"github.com/iodesystems/yscr/decisions"
)

type fakeDecStore struct {
	ds []decisions.Decision
}

func (f *fakeDecStore) Add(d decisions.Decision) (decisions.Decision, error) {
	f.ds = append(f.ds, d)
	return d, nil
}
func (f *fakeDecStore) OpenFor(question, field string) (decisions.Decision, bool, error) {
	for i := len(f.ds) - 1; i >= 0; i-- {
		if f.ds[i].Question == question && f.ds[i].Field == field {
			return f.ds[i], true, nil
		}
	}
	return decisions.Decision{}, false, nil
}
func (f *fakeDecStore) List(statuses ...decisions.Status) ([]decisions.Decision, error) {
	return f.ds, nil
}

func TestListDecisions_ToolRecallsWithProvenance(t *testing.T) {
	c := New(nil, nil).WithDecisions(&fakeDecStore{ds: []decisions.Decision{
		{Question: "Which environment?", Field: "env", Answer: "production", Context: "claude-code·abc"},
		{Question: "Region?", Field: "region", Answer: "eu-west-1", Context: "pwa tap"},
	}})

	out := c.decisionDispatch(context.Background(), "list_decisions", map[string]any{})
	if !containsAll(out, "Which environment?", "production", "claude-code·abc", "Region?", "eu-west-1") {
		t.Fatalf("recall missing provenance: %s", out)
	}
}

func TestListDecisions_Filter(t *testing.T) {
	c := New(nil, nil).WithDecisions(&fakeDecStore{ds: []decisions.Decision{
		{Question: "Which environment?", Field: "env", Answer: "production"},
		{Question: "Region?", Field: "region", Answer: "eu-west-1"},
	}})

	out := c.decisionDispatch(context.Background(), "list_decisions", map[string]any{"filter": "region"})
	if !strings.Contains(out, `"Region?" = eu-west-1`) || strings.Contains(out, "environment") {
		t.Fatalf("filter wrong: %q", out)
	}
	out = c.decisionDispatch(context.Background(), "list_decisions", map[string]any{"filter": "nope"})
	if out != `no recorded decisions matching "nope".` {
		t.Fatalf("empty filter wrong: %q", out)
	}
}

func TestListDecisions_NoStore(t *testing.T) {
	c := New(nil, nil) // no WithDecisions
	out := c.decisionDispatch(context.Background(), "list_decisions", map[string]any{})
	if out != "the decision log is not configured (no durable store)." {
		t.Fatalf("got %q", out)
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		found := false
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
