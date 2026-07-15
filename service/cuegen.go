package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"

	"github.com/iodesystems/yscr/cue"
	"github.com/iodesystems/yscr/source"
)

// cueGenerator is phase 4: the LLM tick that PROPOSES tasks. On a slow cadence it
// shows the model the fleet + standing goals and asks for concrete next tasks,
// then enqueues each into the durable cue. It never dispatches — the
// deterministic release loop (cueRunner) does that, behind its rails. Enqueuing
// is side-effect-free w.r.t. sessions, so the generator runs whenever the cue is
// enabled (even in release dry-run: you see what it would queue and dispatch).
//
// Dedup lives in the store: EnqueueTask skips a task whose DedupeKey already has
// a live (pending|inflight) row, so re-proposing the same work each tick is a
// no-op until that work completes.

const genTimeout = 60 * time.Second

type cueEnqueuer interface {
	EnqueueTask(ctx context.Context, t cue.Task, created int64) (bool, error)
}

type cueGenerator struct {
	runner   agent.LLMRunner
	enq      cueEnqueuer
	fleet    func(ctx context.Context) []source.State
	goals    []string
	interval time.Duration
}

func newCueGenerator(cfg configCueGen, runner agent.LLMRunner, enq cueEnqueuer, fleet func(context.Context) []source.State) *cueGenerator {
	if runner == nil || enq == nil || len(cfg.Goals) == 0 {
		if runner != nil && enq != nil {
			log.Printf("cue: generator idle — no standing goals configured (cue.goals)")
		}
		return nil
	}
	log.Printf("cue: generator ENABLED — %d goal(s), every %ds", len(cfg.Goals), cfg.GenInterval)
	return &cueGenerator{
		runner:   runner,
		enq:      enq,
		fleet:    fleet,
		goals:    cfg.Goals,
		interval: time.Duration(cfg.GenInterval) * time.Second,
	}
}

// configCueGen is the slice of CueConfig the generator needs (keeps the ctor
// signature small + testable).
type configCueGen struct {
	Goals       []string
	GenInterval int
}

// run drives the generator on its cadence until ctx is cancelled.
func (g *cueGenerator) run(ctx context.Context) {
	ticker := time.NewTicker(g.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if n, err := g.generateOnce(ctx); err != nil {
				log.Printf("cue: generate: %v", err)
			} else if n > 0 {
				log.Printf("cue: generator enqueued %d task(s)", n)
			}
		}
	}
}

// generateOnce runs one proposal round and enqueues the (deduped) results,
// returning how many were newly enqueued.
func (g *cueGenerator) generateOnce(ctx context.Context) (int, error) {
	states := g.fleet(ctx)
	ctx, cancel := context.WithTimeout(ctx, genTimeout)
	defer cancel()

	ch, err := g.runner.ChatStream(ctx, []llm.Message{
		{Role: "system", Content: genSystem},
		{Role: "user", Content: g.prompt(states)},
	}, nil, nil)
	if err != nil {
		return 0, err
	}
	var out strings.Builder
	for chunk := range ch {
		if chunk.Error != "" {
			return 0, fmt.Errorf("%s", chunk.Error)
		}
		out.WriteString(chunk.Content)
		if chunk.Done {
			break
		}
	}

	proposals, err := parseProposals(out.String())
	if err != nil {
		return 0, err
	}
	now := time.Now().UnixNano()
	enqueued := 0
	for _, p := range proposals {
		t, ok := p.toTask()
		if !ok {
			continue // skip malformed (no prompt / no source)
		}
		added, err := g.enq.EnqueueTask(ctx, t, now)
		if err != nil {
			log.Printf("cue: enqueue %s: %v", t.ID, err)
			continue
		}
		if added {
			enqueued++
		}
	}
	return enqueued, nil
}

const genSystem = `You are the fleet's work planner. Given the current fleet and the standing goals, propose concrete NEXT tasks that advance the goals.
Rules:
- Only propose work that is not already being done by a session.
- Prefer routing a task to an existing session (set "source" and "session_id") when it is the natural place; otherwise spawn a new one (set "spawn": true, and "dir" if relevant).
- Give each task a stable "dedupe_key" identifying the work, so the same task is not proposed twice.
- "source" MUST be a bare plugin id exactly as shown in the fleet's "source=" field (e.g. "claude-code"), never "source/id". "session_id" is the fleet's "id=" value.
- If nothing new is warranted right now, return an empty list.
Output STRICT JSON only, no prose:
{"tasks":[{"prompt":"...","source":"...","session_id":"...","spawn":false,"dir":"","priority":0,"dedupe_key":"..."}]}`

func (g *cueGenerator) prompt(states []source.State) string {
	var b strings.Builder
	b.WriteString("Standing goals:\n")
	for _, goal := range g.goals {
		fmt.Fprintf(&b, "- %s\n", goal)
	}
	b.WriteString("\nFleet — each line gives the fields to copy into a task verbatim:\n")
	if len(states) == 0 {
		b.WriteString("(no live sessions)\n")
	}
	for _, st := range states {
		title := st.Ref.Title
		if title == "" {
			title = st.Ref.ID
		}
		fmt.Fprintf(&b, "- source=%q id=%q status=%q title=%q — %s\n",
			st.Ref.Source, st.Ref.ID, st.Status, title, oneLine(st.Summary))
	}
	return b.String()
}

// ── proposal parsing ────────────────────────────────────────────────

type genProposal struct {
	Prompt    string `json:"prompt"`
	Source    string `json:"source"`
	SessionID string `json:"session_id"`
	Spawn     bool   `json:"spawn"`
	Dir       string `json:"dir"`
	Priority  int    `json:"priority"`
	DedupeKey string `json:"dedupe_key"`
}

// toTask converts a proposal to a cue.Task, or ok=false if it lacks the minimum
// (a prompt and a source). DedupeKey is derived from the target+prompt when the
// model omits one, so re-proposals still collapse.
func (p genProposal) toTask() (cue.Task, bool) {
	prompt := strings.TrimSpace(p.Prompt)
	src := strings.TrimSpace(p.Source)
	if prompt == "" || src == "" {
		return cue.Task{}, false
	}
	tgt := cue.Target{Source: src, SessionID: strings.TrimSpace(p.SessionID), Spawn: p.Spawn, SpawnDir: p.Dir}
	dedupe := strings.TrimSpace(p.DedupeKey)
	if dedupe == "" {
		dedupe = tgt.Key() + "|" + prompt
	}
	return cue.Task{
		ID:        uuid.NewString(),
		DedupeKey: dedupe,
		Prompt:    prompt,
		Priority:  p.Priority,
		Target:    tgt,
	}, true
}

// parseProposals tolerantly extracts the {"tasks":[...]} object from the model's
// output (it may wrap JSON in prose or code fences).
func parseProposals(s string) ([]genProposal, error) {
	raw := extractJSONObject(s)
	if raw == "" {
		return nil, fmt.Errorf("no JSON object in generator output")
	}
	var doc struct {
		Tasks []genProposal `json:"tasks"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return nil, fmt.Errorf("parse tasks: %w", err)
	}
	return doc.Tasks, nil
}

// extractJSONObject returns the substring from the first '{' to the last '}'.
func extractJSONObject(s string) string {
	i := strings.IndexByte(s, '{')
	j := strings.LastIndexByte(s, '}')
	if i < 0 || j < i {
		return ""
	}
	return s[i : j+1]
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
