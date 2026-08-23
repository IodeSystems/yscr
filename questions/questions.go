// Package questions is the durable open-questions queue: ambiguities the
// concierge parks while it continues work that doesn't depend on them. A
// question is lighter than a source.Questionnaire (free-form, no schema) and
// outlives one conversation turn — it sits in the PWA next to "Needs you"
// until the user answers it (conversationally or by tap).
package questions

import (
	"context"
	"strings"
)

// Question is one parked ambiguity.
type Question struct {
	ID        string
	Question  string // the question, one sentence
	Context   string // why it matters / what is blocked on it
	TaskID    string // optional: the task this blocks (scratchpad/cue id)
	CreatedAt int64  // ns

	// Filled in by the store on reads.
	Status     Status
	Answer     string
	AnsweredAt int64 // ns
}

// Status is a question's lifecycle state.
type Status string

const (
	StatusOpen     Status = "open"
	StatusAnswered Status = "answered"
)

// QuestionsStore is the durable queue. *store.PG satisfies it via an adapter
// (PG also carries scratchpad's Add); tests use fakes.
type QuestionsStore interface {
	// Add inserts an open question. Returns nil when an open question with the
	// same text already exists (dedupe by normalized question text).
	Add(ctx context.Context, q Question) (*Question, error)
	// List returns open questions first (oldest first — they've waited longest),
	// then answered ones (newest first).
	List(ctx context.Context) ([]Question, error)
	// Answer records the user's answer (status-guarded; false for unknown or
	// already-answered ids).
	Answer(ctx context.Context, id, answer string) (bool, error)
}

// Normalize returns the dedupe key for a question: lowercased, whitespace-
// collapsed text. Empty questions have an empty key (Add rejects them).
func Normalize(q Question) string { return normalizeText(q.Question) }

func normalizeText(s string) string {
	fields := strings.Fields(s)
	out := make([]string, len(fields))
	for i, f := range fields {
		out[i] = strings.ToLower(f)
	}
	return strings.Join(out, " ")
}
