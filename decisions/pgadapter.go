package decisions

import (
	"context"
	"fmt"
	"time"
)

// PGAdapter adapts *store.PG's decision methods to Store. The concrete type is
// deliberately NOT named here: the adapter lives in service (which imports both
// packages), keeping this package free of a store dependency. This file only
// holds shared helpers used on that path.

// RenderAnswer renders one field's answer value for storage + display: choice
// values as-is, multi as comma-joined, text trimmed.
func RenderAnswer(v any) string {
	switch a := v.(type) {
	case nil:
		return ""
	case string:
		return trimSpace(a)
	case []any:
		parts := make([]string, 0, len(a))
		for _, p := range a {
			if s, ok := p.(string); ok {
				parts = append(parts, trimSpace(s))
			}
		}
		return joinComma(parts)
	default:
		return fmt.Sprintf("%v", a)
	}
}

func trimSpace(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}

func joinComma(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}

// Since renders a Unix timestamp the way provenance lines read it.
func Since(unix int64) string { return time.Unix(unix, 0).Format("2006-01-02") }

var _ = context.Background
