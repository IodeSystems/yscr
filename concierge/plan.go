// plan.go wires the goal-plan tool: plan_goal(goal) batch-decomposes a goal
// into a durable task graph in one call. The model proposes tasks + dependency
// edges; the deterministic layer validates (known ids, acyclic) and enqueues —
// the same "LLM proposes, deterministic layer decides" line as the cue
// generator. Unlike the generator tick (which proposes single next-steps on a
// cadence), this is user-initiated: "get me to X" → a whole plan at once.
package concierge

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/iodesystems/agentkit/llm"

	"github.com/iodesystems/yscr/cue"
)

// cueEnqueuer is the slice of the durable store plan_goal needs.
type cueEnqueuer interface {
	EnqueueTask(ctx context.Context, t cue.Task, created int64) (bool, error)
}

// WithPlanGoal attaches the plan_goal tool. A nil enqueuer leaves the toolset
// unchanged (no Postgres → no durable cue to plan into).
func (c *Concierge) WithPlanGoal(enq cueEnqueuer) *Concierge {
	c.planEnq = enq
	if enq != nil {
		c.planOn = true
	}
	return c
}

var planToolDef = func() llm.ToolDef {
	taskProps := map[string]any{
		"id":         strProp("short slug id for this task, used by deps"),
		"prompt":     strProp("what to hand the target session"),
		"source":     strProp("target source id (e.g. claude-code)"),
		"session_id": strProp("existing session id within that source"),
		"spawn":      map[string]any{"type": "boolean", "description": "true = run in a NEW session"},
		"dir":        strProp("working directory for a spawn"),
		"priority":   map[string]any{"type": "integer", "description": "higher is scheduled first (default 0)"},
		"deps":       map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "ids of tasks that must be done first"},
	}
	return toolDef("plan_goal", "Decompose a goal into a PLAN: several concrete tasks with dependencies, enqueued as one batch (durable; the fleet release loop dispatches them as capacity allows). Use when the user says 'get me to X', 'plan how to…', or names multi-step work. Each task needs a prompt and a target (an existing session's source+id, or spawn=true for a new claude-code session). If one task must finish before another, set the later task's deps to the earlier task's id — ids are short slugs you assign, used only within this batch, never cyclic.", objSchema(map[string]any{
		"goal":  strProp("the goal in one sentence"),
		"tasks": map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": taskProps, "required": []string{"id", "prompt", "source"}}, "description": "the plan, 2-8 tasks; more only if the goal genuinely needs it"},
	}, "goal", "tasks"))
}()

// planDispatch runs one plan_goal call: validate the batch (ids known + acyclic),
// enqueue each task, and report what landed. Bad edges are relaxed (deps
// dropped) rather than failing the whole plan — work is never lost.
func (c *Concierge) planDispatch(ctx context.Context, args map[string]any) string {
	if c.planEnq == nil {
		return "planning is not configured (no durable store)."
	}
	goal, _ := args["goal"].(string)
	raw, _ := args["tasks"].([]any)
	if strings.TrimSpace(goal) == "" || len(raw) == 0 {
		return "plan_goal needs a goal and at least one task."
	}

	batch := make([]cue.Task, 0, len(raw))
	for _, item := range raw {
		o, ok := item.(map[string]any)
		if !ok {
			continue
		}
		prompt, _ := o["prompt"].(string)
		src, _ := o["source"].(string)
		if strings.TrimSpace(prompt) == "" || strings.TrimSpace(src) == "" {
			continue // skip malformed entries
		}
		id, _ := o["id"].(string)
		id = strings.TrimSpace(id)
		if id == "" {
			id = uuid.NewString()
		}
		spawn, _ := o["spawn"].(bool)
		sid, _ := o["session_id"].(string)
		dir, _ := o["dir"].(string)
		prio, _ := o["priority"].(float64)
		deps := []string{}
		if dl, ok := o["deps"].([]any); ok {
			for _, d := range dl {
				if s, ok := d.(string); ok && strings.TrimSpace(s) != "" {
					deps = append(deps, strings.TrimSpace(s))
				}
			}
		}
		batch = append(batch, cue.Task{
			ID:        id,
			DedupeKey: "plan|" + id + "|" + prompt,
			Prompt:    strings.TrimSpace(prompt),
			Priority:  int(prio),
			Deps:      deps,
			Target:    cue.Target{Source: src, SessionID: sid, Spawn: spawn, SpawnDir: dir},
		})
	}
	if len(batch) == 0 {
		return "no valid tasks in the plan (each needs a prompt and a source)."
	}

	// Deterministic validation: deps must name batch ids; the graph must be
	// acyclic. A bad edge drops its deps (the task still enqueues).
	byID := make(map[string]bool, len(batch))
	for _, t := range batch {
		byID[t.ID] = true
	}
	var dropped []string
	for i := range batch {
		ok := true
		for _, d := range batch[i].Deps {
			if !byID[d] {
				ok = false
				break
			}
		}
		if !ok {
			dropped = append(dropped, batch[i].ID)
			batch[i].Deps = nil
		}
	}
	if err := cue.ValidateDeps(batch); err != nil {
		// Cycle: relax edge-by-edge (drop one task's deps at a time) until
		// acyclic — mirroring the generator's posture.
		for i := range batch {
			if len(batch[i].Deps) == 0 {
				continue
			}
			saved := batch[i].Deps
			batch[i].Deps = nil
			cand := make([]cue.Task, len(batch))
			copy(cand, batch)
			if cue.ValidateDeps(cand) == nil {
				dropped = append(dropped, batch[i].ID)
			} else {
				batch[i].Deps = saved
			}
		}
	}

	now := time.Now().UnixNano()
	var enq, dup int
	for _, t := range batch {
		added, err := c.planEnq.EnqueueTask(ctx, t, now)
		if err != nil {
			return fmt.Sprintf("enqueue failed for %q: %v", t.ID, err)
		}
		if added {
			enq++
		} else {
			dup++
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "plan enqueued: %d task(s)", enq)
	if dup > 0 {
		fmt.Fprintf(&b, ", %d already live (deduped)", dup)
	}
	if len(dropped) > 0 {
		fmt.Fprintf(&b, "; deps relaxed on %s (unknown or cyclic)", strings.Join(dropped, ", "))
	}
	b.WriteString(". The release loop dispatches them as sessions free up; use render_diagram(kind=tasks) to show the graph.")
	return b.String()
}
