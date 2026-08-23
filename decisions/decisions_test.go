package decisions

import (
	"strings"
	"testing"
)

func TestKeyFor_Normalizes(t *testing.T) {
	k1, _ := KeyFor("  Which  region? ", "region")
	k2, _ := KeyFor("which region?", "REGION")
	if k1 != k2 {
		t.Fatalf("normalization should match: %s vs %s", k1, k2)
	}
	k3, _ := KeyFor("Which zone?", "region")
	if k1 == k3 {
		t.Fatal("different questions must not collide")
	}
	k4, _ := KeyFor("Which region?", "")
	if k1 == k4 {
		t.Fatal("field scoping lost")
	}
}

func TestAddSupersedes(t *testing.T) {
	m := NewMem()
	k, _ := KeyFor("Which region?", "region")
	d1, _ := m.Add(Decision{QuestionKey: k, Question: "which region?", Field: "region", Answer: "eu"})
	d2, _ := m.Add(Decision{QuestionKey: k, Question: "which region?", Field: "region", Answer: "us"})
	if d1.Status == StatusSuperseded || d2.Status != StatusOpen {
		t.Fatal("second add must supersede the first")
	}
	d, ok, _ := m.OpenFor("Which region?", "region")
	if !ok || d.Answer != "us" {
		t.Fatalf("OpenFor wrong: %+v", d)
	}
	rows, _ := m.List()
	if len(rows) != 2 {
		t.Fatalf("append-only: want 2 rows, got %d", len(rows))
	}
	open, _ := m.List(StatusOpen)
	if len(open) != 1 || open[0].Answer != "us" {
		t.Fatalf("open row wrong: %+v", open)
	}
	if !ok {
		t.Fatal("OpenFor should find the open decision by key")
	}
}

func TestResolve_ExactOnly(t *testing.T) {
	m := NewMem()
	qp, _ := KeyFor("Which region?", "region")
	m.Add(Decision{QuestionKey: qp, Question: "which region?", Field: "region", Answer: "eu"})
	q := &Questionnaire{Fields: []Field{{Key: "region", Prompt: " which REGION? "}, {Key: "env", Prompt: "Which env?"}}}
	ans, applied, err := Resolve(q, m)
	if err != nil {
		t.Fatal(err)
	}
	if len(ans) != 1 || ans["region"] != "eu" {
		t.Fatalf("want only region=eu, got %v", ans)
	}
	if len(applied) != 1 || !strings.Contains(applied[0], "decided") {
		t.Fatalf("applied provenance wrong: %v", applied)
	}
}

func TestResolve_EmptyWhenUndecided(t *testing.T) {
	m := NewMem()
	q := &Questionnaire{Fields: []Field{{Key: "a", Prompt: "A?"}}}
	ans, applied, _ := Resolve(q, m)
	if ans != nil || len(applied) != 0 {
		t.Fatalf("want nothing, got %v %v", ans, applied)
	}
}
