package service

import (
	"context"

	"log"
	"time"

	"github.com/iodesystems/yscr/decisions"
	"github.com/iodesystems/yscr/source"
	"github.com/iodesystems/yscr/store"
)

// pgDecisions adapts *store.PG's decision methods to decisions.Store. (The
// concrete PG can't satisfy the interface directly: it also has scratchpad's
// Add.)
type pgDecisions struct{ pg *store.PG }

func (a *pgDecisions) Add(d decisions.Decision) (decisions.Decision, error) {
	ctx := context.Background()
	created := d.CreatedAt.Unix()
	out, err := a.pg.AddDecision(ctx, store.Decision{
		ID: d.ID, QuestionKey: d.QuestionKey, Question: d.Question, Field: d.Field,
		Answer: d.Answer, Context: d.Context, Status: string(d.Status), CreatedAt: created,
	})
	if err != nil {
		return decisions.Decision{}, err
	}
	d.ID = out.ID
	return d, nil
}

func (a *pgDecisions) OpenFor(question, field string) (decisions.Decision, bool, error) {
	key, norm := decisions.KeyFor(question, field)
	d, ok, err := a.pg.OpenDecision(context.Background(), key)
	if !ok || err != nil {
		return decisions.Decision{}, ok, err
	}
	return decisions.Decision{
		ID: d.ID, QuestionKey: d.QuestionKey, Question: norm, Field: d.Field,
		Answer: d.Answer, Context: d.Context, Status: decisions.Status(d.Status),
		CreatedAt: timeUnix(d.CreatedAt),
	}, true, nil
}

func (a *pgDecisions) List(statuses ...decisions.Status) ([]decisions.Decision, error) {
	var args []string
	for _, s := range statuses {
		args = append(args, string(s))
	}
	rows, err := a.pg.ListDecisions(context.Background(), args...)
	if err != nil {
		return nil, err
	}
	out := make([]decisions.Decision, 0, len(rows))
	for _, d := range rows {
		out = append(out, decisions.Decision{
			ID: d.ID, QuestionKey: d.QuestionKey, Question: d.Question, Field: d.Field,
			Answer: d.Answer, Context: d.Context, Status: decisions.Status(d.Status),
			CreatedAt: timeUnix(d.CreatedAt), SupersededBy: d.SupersededBy,
		})
	}
	return out, nil
}

// logAnswers records every field of an answered questionnaire into the decision
// log. Deterministic — no model in the path. Best-effort: a logging failure
// must never fail the answer itself.
func (s *Server) logAnswers(q *source.Questionnaire, answers map[string]any, ctxLabel string) {
	if s.pad == nil {
		return
	}
	st := &pgDecisions{pg: s.pad.(*store.PG)}
	for _, f := range q.Fields {
		v, ok := answers[f.Key]
		if !ok {
			continue
		}
		key, norm := decisions.KeyFor(f.Prompt, f.Key)
		d, err := st.Add(decisions.Decision{
			QuestionKey: key, Question: norm, Field: f.Key,
			Answer:  decisions.RenderAnswer(v),
			Context: ctxLabel, Status: decisions.StatusOpen,
		})
		if err != nil {
			s.logf("decisions: log %s/%s: %v", f.Prompt, f.Key, err)
			continue
		}
		s.logf("decisions: logged %q = %s (%s)", f.Prompt, d.Answer, ctxLabel)
	}
}

// resolveKnown answers the fields of a questionnaire that are already decided,
// returning the deterministic auto-resolve. The caller decides whether to apply
// them (exact matches only — never inference).
func (s *Server) resolveKnown(q *source.Questionnaire) (map[string]string, []string) {
	if s.pad == nil {
		return nil, nil
	}
	st := &pgDecisions{pg: s.pad.(*store.PG)}
	dq := &decisions.Questionnaire{}
	for _, f := range q.Fields {
		dq.Fields = append(dq.Fields, decisions.Field{Key: f.Key, Prompt: f.Prompt})
	}
	ans, applied, err := decisions.Resolve(dq, st)
	if err != nil {
		s.logf("decisions: resolve: %v", err)
		return nil, nil
	}
	return ans, applied
}

func (s *Server) logf(format string, args ...any) { log.Printf("yscr "+format, args...) }

func timeUnix(unix int64) time.Time { return time.Unix(unix, 0) }
