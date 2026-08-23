// Package decisions is the decision log: a durable record of the choices the
// user makes, so later questions whose answer is already obvious from those
// preferences can be resolved automatically — with a log of what was decided,
// and why.
//
// The design line (LLM proposes, deterministic layer validates) applies here
// twice: capture is DETERMINISTIC (every answered questionnaire is logged as-is,
// no model in the path), and exact-match resolution is DETERMINISTIC (same
// question hash + same field → the recorded answer). Near-match / preference-
// inference by an LLM is a separate, clearly-labeled step the caller performs —
// this package only ever resolves what it can prove.
package decisions

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// Status is where a decision stands. Append-only + supersede: answering the
// same question again doesn't delete the old row — it SUPERSEDES it.
type Status string

const (
	StatusOpen       Status = "open"
	StatusSuperseded Status = "superseded"
)

// Decision is one recorded choice: what was chosen, for which question, when,
// in which session.
type Decision struct {
	ID           string
	QuestionKey  string // stable hash of the normalized question+field (see KeyFor)
	Question     string // the field prompt, normalized
	Field        string // field key within the questionnaire ("" if single-field)
	Answer       string // the chosen value(s), rendered for humans + matching
	Context      string // where it came from: source·session·questionnaire id
	Status       Status
	CreatedAt    time.Time
	SupersededBy string // ID of the decision that replaced this one ("" if open)
}

// Questionnaire is the minimal shape Resolve needs — deliberately local so this
// package stays import-free of the rest of the repo (same posture as reports).
type Questionnaire struct {
	Fields []Field
}

type Field struct {
	Key    string
	Prompt string
}

// KeyFor is the stable identity of a question: field-scoped, normalized for
// whitespace/case so re-wording by spacing or caps still matches. The hash is
// what's compared; the normalized text is stored for display.
func KeyFor(question, field string) (key, norm string) {
	norm = normalize(question) + "\x00" + strings.ToLower(strings.TrimSpace(field))
	sum := sha256.Sum256([]byte(norm))
	return hex.EncodeToString(sum[:16]), norm
}

func normalize(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	for strings.Contains(s, "  ") {
		s = strings.ReplaceAll(s, "  ", " ")
	}
	return s
}

// Store persists decisions. Implementations: *Mem (this package), and the
// service-level adapter over *store.PG (service/decisions.go).
type Store interface {
	// Add records a decision and supersedes any open one for the same
	// question+field. Returns the new decision.
	Add(d Decision) (Decision, error)
	// OpenFor returns the current (open) decision for a question+field, if any.
	OpenFor(question, field string) (Decision, bool, error)
	// List returns decisions newest-first; empty statuses means all.
	List(statuses ...Status) ([]Decision, error)
}

// Resolve is the deterministic auto-resolve: given a questionnaire's pending
// fields, return answers that are already decided — only exact question+field
// matches, never inference. The caller decides whether to apply them (and must
// log that it did, via Add with Context noting the provenance).
func Resolve(q *Questionnaire, st Store) (map[string]string, []string, error) {
	var out map[string]string
	var applied []string
	for _, f := range q.Fields {
		d, ok, err := st.OpenFor(f.Prompt, f.Key)
		if err != nil || !ok {
			continue
		}
		if out == nil {
			out = map[string]string{}
		}
		out[f.Key] = d.Answer
		applied = append(applied, fmt.Sprintf("%s=%q (decided %s)", f.Key, d.Answer, d.CreatedAt.Format("2006-01-02")))
	}
	return out, applied, nil
}
