package service

import (
	"testing"

	"github.com/iodesystems/yscr/decisions"
	"github.com/iodesystems/yscr/source"
)

func TestLogAnswersAndResolve(t *testing.T) {
	s := &Server{} // pad nil → no-op, must not panic
	q := &source.Questionnaire{Fields: []source.Field{{Key: "region", Prompt: "Which region?"}}}
	s.logAnswers(q, map[string]any{"region": "eu"}, "test")
	if ans, applied := s.resolveKnown(q); ans != nil || len(applied) != 0 {
		t.Fatalf("no store → nothing resolved: %v %v", ans, applied)
	}
}

func TestRenderAnswer(t *testing.T) {
	if got := decisions.RenderAnswer("eu"); got != "eu" {
		t.Fatal(got)
	}
	if got := decisions.RenderAnswer([]any{"email", "sms"}); got != "email, sms" {
		t.Fatal(got)
	}
	if got := decisions.RenderAnswer(nil); got != "" {
		t.Fatal(got)
	}
}

func TestKeyForStableAcrossCaseAndSpace(t *testing.T) {
	a, _ := decisions.KeyFor("  Which   region? ", "region")
	b, _ := decisions.KeyFor("which region?", "REGION")
	if a != b {
		t.Fatalf("%s != %s", a, b)
	}
}
