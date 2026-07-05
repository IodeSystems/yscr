// Package concierge is the fleet concierge itself: an agentkit session with a
// source-aware toolset. It observes and drives every registered source through
// the source.Source contract, and talks to the user through the membrane.
//
// The LLM endpoint is swappable — the caller passes any agent.LLMRunner
// (llm.NewClient pointed at corrallm / OpenRouter / a claude-code-tmux bridge).
// The concierge owns its OWN conversation store (yscr's, not autowork3's).
package concierge

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"

	"github.com/iodesystems/yscr/source"
)

// Concierge holds the registered sources + the LLM plumbing for one membrane.
type Concierge struct {
	sources map[string]source.Source
	order   []string // stable source iteration order
	store   agent.Store
	runner  agent.LLMRunner
	system  string
}

// New builds a concierge over a runner (the swappable LLM endpoint), a store
// (its conversation persistence), and the registered source plugins.
func New(runner agent.LLMRunner, st agent.Store, sources ...source.Source) *Concierge {
	c := &Concierge{sources: map[string]source.Source{}, store: st, runner: runner, system: DefaultSystem}
	for _, s := range sources {
		if _, dup := c.sources[s.ID()]; !dup {
			c.order = append(c.order, s.ID())
		}
		c.sources[s.ID()] = s
	}
	sort.Strings(c.order)
	return c
}

// WithSystem overrides the system prompt.
func (c *Concierge) WithSystem(s string) *Concierge { c.system = s; return c }

// Converse feeds one user message into the membrane and returns the
// concierge's spoken reply. The concierge may call source tools (fleet_status,
// pull_detail, post, spawn) before replying.
func (c *Concierge) Converse(ctx context.Context, sessionID, userMessage string) (string, error) {
	sess := c.session(sessionID)
	if err := sess.Inject(ctx, agent.Entry{Kind: agent.KindUser, Content: userMessage}); err != nil {
		return "", err
	}
	res, err := sess.Turn(ctx)
	if err != nil {
		return "", err
	}
	return res.Reply, nil
}

func (c *Concierge) session(sessionID string) *agent.Session {
	return &agent.Session{
		SessionID:  sessionID,
		System:     c.system,
		Store:      c.store,
		Runner:     c.runner,
		Tools:      conciergeTools,
		Dispatch:   c.dispatch,
		SpanPrefix: "concierge",
	}
}

// ── tools ───────────────────────────────────────────────────────────

func objSchema(props map[string]any, required ...string) map[string]any {
	return map[string]any{"type": "object", "properties": props, "required": required}
}
func strProp(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }

func toolDef(name, desc string, params map[string]any) llm.ToolDef {
	var td llm.ToolDef
	td.Type = "function"
	td.Function.Name = name
	td.Function.Description = desc
	td.Function.Parameters = params
	return td
}

var conciergeTools = []llm.ToolDef{
	toolDef("fleet_status", "List every live session across all sources with a one-line status. Call this first when the user asks what's going on.", objSchema(map[string]any{})),
	toolDef("pull_detail", "Get the detailed rollup for one session (status, blockers, and any questionnaires awaiting the user).", objSchema(map[string]any{
		"source": strProp("the source id, e.g. autowork"),
		"id":     strProp("the session id within that source"),
	}, "source", "id")),
	toolDef("post", "Send a message into a session on the user's behalf.", objSchema(map[string]any{
		"source":  strProp("the source id"),
		"id":      strProp("the session id"),
		"message": strProp("the message to post"),
	}, "source", "id", "message")),
	toolDef("spawn", "Start new work in a source (a new thread/session).", objSchema(map[string]any{
		"source": strProp("the source id"),
		"title":  strProp("a short title for the work"),
		"prompt": strProp("what the work should do — the first instruction"),
	}, "source", "prompt")),
	toolDef("answer_questionnaire", "Submit the user's answers to a questionnaire awaiting them (a decision). Gather the answers CONVERSATIONALLY first, then submit them here as {field_key: value}. A choice value must be one of that field's listed options.", objSchema(map[string]any{
		"source":           strProp("the source id"),
		"id":               strProp("the session id"),
		"questionnaire_id": strProp("the questionnaire id from pull_detail"),
		"answers":          map[string]any{"type": "object", "description": "map of field_key → the user's chosen value"},
	}, "source", "id", "questionnaire_id", "answers")),
}

func (c *Concierge) dispatch(ctx context.Context, tc llm.ToolCall) (string, error) {
	var args map[string]any
	if tc.Function.Arguments != "" {
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			return fmt.Sprintf("invalid arguments for %s: %v", tc.Function.Name, err), nil
		}
	}
	str := func(k string) string { s, _ := args[k].(string); return s }

	switch tc.Function.Name {
	case "fleet_status":
		return c.fleetStatus(ctx), nil
	case "pull_detail":
		return c.pullDetail(ctx, str("source"), str("id")), nil
	case "post":
		src, ok := c.sources[str("source")]
		if !ok {
			return unknownSource(str("source")), nil
		}
		if err := src.Post(ctx, str("id"), str("message")); err != nil {
			return fmt.Sprintf("post failed: %v", err), nil
		}
		return "posted.", nil
	case "spawn":
		src, ok := c.sources[str("source")]
		if !ok {
			return unknownSource(str("source")), nil
		}
		sp, ok := src.(source.Spawner)
		if !ok {
			return fmt.Sprintf("source %q cannot spawn work.", str("source")), nil
		}
		ref, err := sp.Spawn(ctx, source.SpawnSpec{Title: str("title"), Prompt: str("prompt")})
		if err != nil {
			return fmt.Sprintf("spawn failed: %v", err), nil
		}
		return fmt.Sprintf("started %s/%s (%s).", ref.Source, ref.ID, ref.Title), nil
	case "answer_questionnaire":
		return c.answerQuestionnaire(ctx, str("source"), str("id"), str("questionnaire_id"), args["answers"]), nil
	default:
		return fmt.Sprintf("unknown tool %q.", tc.Function.Name), nil
	}
}

func unknownSource(id string) string { return fmt.Sprintf("no source %q registered.", id) }

// fleetStatus lists every source's sessions with a one-line rollup.
func (c *Concierge) fleetStatus(ctx context.Context) string {
	var b strings.Builder
	total := 0
	for _, sid := range c.order {
		src := c.sources[sid]
		refs, err := src.List(ctx)
		if err != nil {
			fmt.Fprintf(&b, "[%s] error: %v\n", sid, err)
			continue
		}
		for _, ref := range refs {
			total++
			st, err := src.State(ctx, ref.ID)
			if err != nil {
				fmt.Fprintf(&b, "- %s/%s: (state error: %v)\n", sid, ref.ID, err)
				continue
			}
			title := ref.Title
			if title == "" {
				title = "(untitled)"
			}
			fmt.Fprintf(&b, "- %s/%s %q [%s]: %s\n", sid, ref.ID, title, st.Status, st.Summary)
		}
	}
	if total == 0 {
		return "Nothing active across any source."
	}
	return fmt.Sprintf("%d live session(s):\n%s", total, b.String())
}

// pullDetail returns one session's full rollup incl. any questionnaires.
func (c *Concierge) pullDetail(ctx context.Context, sourceID, id string) string {
	src, ok := c.sources[sourceID]
	if !ok {
		return unknownSource(sourceID)
	}
	st, err := src.State(ctx, id)
	if err != nil {
		return fmt.Sprintf("could not read %s/%s: %v", sourceID, id, err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s/%s [%s]: %s\n", sourceID, id, st.Status, st.Summary)
	for _, bl := range st.Blockers {
		fmt.Fprintf(&b, "  blocker: %s\n", bl)
	}
	for _, q := range st.Pending {
		fmt.Fprintf(&b, "  awaiting you — %s (id=%s):\n", q.Title, q.ID)
		for _, f := range q.Fields {
			opts := ""
			if len(f.Options) > 0 {
				var vs []string
				for _, o := range f.Options {
					vs = append(vs, o.Value)
				}
				opts = " {" + strings.Join(vs, "|") + "}"
			}
			fmt.Fprintf(&b, "    - %s: %s%s\n", f.Key, f.Prompt, opts)
		}
	}
	return b.String()
}

// answerQuestionnaire is the form↔conversation submit: re-fetch the live
// questionnaire, validate the assembled answers against it (returning a fix
// instruction on failure so the model re-asks the user — the fix loop), then
// hand the validated Answer to the source's Actor.
func (c *Concierge) answerQuestionnaire(ctx context.Context, sourceID, id, qid string, rawAnswers any) string {
	src, ok := c.sources[sourceID]
	if !ok {
		return unknownSource(sourceID)
	}
	actor, ok := src.(source.Actor)
	if !ok {
		return fmt.Sprintf("source %q cannot accept answers.", sourceID)
	}
	answers, _ := rawAnswers.(map[string]any)
	if answers == nil {
		return "answers must be an object of {field_key: value}."
	}

	st, err := src.State(ctx, id)
	if err != nil {
		return fmt.Sprintf("could not read %s/%s: %v", sourceID, id, err)
	}
	var q *source.Questionnaire
	for i := range st.Pending {
		if st.Pending[i].ID == qid {
			q = &st.Pending[i]
			break
		}
	}
	if q == nil {
		return fmt.Sprintf("no questionnaire %q is awaiting on %s/%s (it may already be resolved).", qid, sourceID, id)
	}
	if err := source.Validate(*q, answers); err != nil {
		return fmt.Sprintf("answers not ready: %v. Ask the user for the missing/invalid answers, then resubmit.", err)
	}

	res, err := actor.Act(ctx, id, source.Action{
		Name: "answer_questionnaire",
		Args: map[string]any{"questionnaire_id": qid, "answers": answers},
	})
	if err != nil {
		return fmt.Sprintf("submit failed: %v", err)
	}
	return res
}
