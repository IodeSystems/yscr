package source

import (
	"strings"
	"testing"
)

func decisionQ() Questionnaire {
	return Questionnaire{
		ID:    "req-1",
		Title: "Triage",
		Fields: []Field{
			{Key: "a", Prompt: "Reply?", Type: FieldChoice, Required: true,
				Options: []Option{{Value: "apply"}, {Value: "dismiss"}}},
			{Key: "b", Prompt: "Archive?", Type: FieldChoice, Required: true,
				Options: []Option{{Value: "apply"}, {Value: "dismiss"}}},
		},
	}
}

func TestValidate(t *testing.T) {
	q := decisionQ()

	if err := Validate(q, map[string]any{"a": "apply", "b": "dismiss"}); err != nil {
		t.Fatalf("valid answers rejected: %v", err)
	}

	err := Validate(q, map[string]any{"a": "apply"})
	if err == nil || !strings.Contains(err.Error(), "b is required") {
		t.Errorf("missing required not caught: %v", err)
	}

	err = Validate(q, map[string]any{"a": "maybe", "b": "dismiss"})
	if err == nil || !strings.Contains(err.Error(), `a="maybe" is not an allowed option`) {
		t.Errorf("bad choice not caught: %v", err)
	}
}
