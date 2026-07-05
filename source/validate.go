package source

import (
	"fmt"
	"strings"
)

// Validate checks a set of answers against a Questionnaire's fields: every
// required field must be present + non-empty, and choice/multi values must be
// among the field's Options. It returns a single error listing ALL problems —
// the fix instruction the concierge feeds back to the model so it re-asks the
// user and resubmits (the form↔conversation fix loop). nil = ready to submit.
func Validate(q Questionnaire, answers map[string]any) error {
	allowed := map[string]map[string]bool{}
	for _, f := range q.Fields {
		if len(f.Options) > 0 {
			set := make(map[string]bool, len(f.Options))
			for _, o := range f.Options {
				set[o.Value] = true
			}
			allowed[f.Key] = set
		}
	}

	var probs []string
	for _, f := range q.Fields {
		v, present := answers[f.Key]
		if !present || v == nil || v == "" {
			if f.Required {
				probs = append(probs, fmt.Sprintf("%s is required", f.Key))
			}
			continue
		}
		switch f.Type {
		case FieldChoice:
			s, _ := v.(string)
			if set := allowed[f.Key]; set != nil && !set[s] {
				probs = append(probs, fmt.Sprintf("%s=%q is not an allowed option", f.Key, s))
			}
		case FieldMulti:
			for _, item := range toStrings(v) {
				if set := allowed[f.Key]; set != nil && !set[item] {
					probs = append(probs, fmt.Sprintf("%s contains %q which is not an allowed option", f.Key, item))
				}
			}
		}
	}
	if len(probs) > 0 {
		return fmt.Errorf("%s", strings.Join(probs, "; "))
	}
	return nil
}

// toStrings coerces a JSON-decoded multi value ([]any of strings, []string, or
// a lone string) into a string slice.
func toStrings(v any) []string {
	switch x := v.(type) {
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return x
	case string:
		return []string{x}
	}
	return nil
}
