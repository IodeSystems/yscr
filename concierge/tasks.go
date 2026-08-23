// tasks.go wires the scratchpad (the user-facing work-list) into the concierge
// as three tools: add_task / list_tasks / done_task. The model proposes; the
// scratchpad.Store validates + dedupes + persists — the same "LLM proposes,
// deterministic layer decides" line as the cue pipeline.
package concierge

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/iodesystems/agentkit/llm"

	"github.com/iodesystems/yscr/scratchpad"

	"github.com/iodesystems/yscr/source")

// WithTasks attaches the scratchpad tools. A nil store leaves the toolset
// unchanged (no DSN → no durable work-list).
func (c *Concierge) WithTasks(st scratchpad.Store) *Concierge {
	c.tasks = st
	if st != nil {
		c.taskToolsOn = true
	}
	return c
}

var taskToolDefs = []llm.ToolDef{
	toolDef("add_task", "Add a task or todo to the user's work-list (durable; survives restart). Use when the user says 'add a todo', 'remind me to…', or names work that should be tracked. Set cron for a repeating schedule ('0 9 * * *') and/or run_at_epoch_ms for a one-shot time. If the work should go to a specific session, set source/id (or spawn=true + dir for a new claude-code session).", objSchema(map[string]any{
		"prompt":          strProp("what the task is — the todo text or instruction"),
		"kind":            map[string]any{"type": "string", "enum": []string{"todo", "command"}, "description": "todo (default) or command (a shell command to run)"},
		"priority":        map[string]any{"type": "integer", "description": "higher is scheduled first (default 0)"},
		"cron":            strProp("optional 5-field cron for a repeating task, e.g. '0 9 * * *'"),
		"run_at_epoch_ms": map[string]any{"type": "integer", "description": "optional one-shot time as epoch milliseconds"},
		"source":          strProp("optional target source id (e.g. claude-code) for promoted work"),
		"id":              strProp("optional target session id within that source"),
		"spawn":           map[string]any{"type": "boolean", "description": "true to run the work in a NEW session instead of an existing one"},
		"dir":             strProp("working directory for a spawn (claude-code)"),
	}, "prompt")),
	toolDef("list_tasks", "List the user's work-list: open tasks first, closed after. Use when the user asks what's on their list / what's pending.", objSchema(map[string]any{
		"kind": strProp("optional filter: todo | command (empty = all)"),
	})),
	toolDef("done_task", "Mark a task completed (or failed) by its id from list_tasks. Use when the user says a task is done.", objSchema(map[string]any{
		"id":   strProp("the task id"),
		"done": map[string]any{"type": "boolean", "description": "true = completed, false = failed (default true)"},
	}, "id")),
}

// ── dispatch ────────────────────────────────────────────────────────

func (c *Concierge) taskDispatch(ctx context.Context, name string, args map[string]any) string {
	if c.tasks == nil {
		return "the work-list is not configured (no durable store)."
	}
	str := func(k string) string { s, _ := args[k].(string); return s }

	switch name {
	case "add_task":
		t := scratchpad.Task{
			ID:        uuid.NewString(),
			Prompt:    str("prompt"),
			Kind:      scratchpad.KindTodo,
			CreatedAt: time.Now().UnixNano(),
		}
		if k := str("kind"); k == "command" {
			t.Kind = scratchpad.KindCommand
		}
		if n, ok := args["priority"].(float64); ok {
			t.Priority = int(n)
		}
		t.Cron = str("cron")
		if ms, ok := args["run_at_epoch_ms"].(float64); ok && ms > 0 {
			t.RunAt = int64(ms) * int64(time.Millisecond)
		}
		src := str("source")
		if src != "" {
			if _, known := c.sources[src]; !known {
				return unknownSource(src)
			}
			t.Target = scratchpad.Target{
				Source:    src,
				SessionID: str("id"),
				Spawn:     boolArg(args, "spawn"),
				SpawnDir:  str("dir"),
			}
			if t.Target.Spawn && t.Target.Source == "claude-code" && t.Target.SpawnDir == "" {
				return "a claude-code spawn target needs a dir (absolute path to launch in)."
			}
		}
		stored, err := c.tasks.Add(ctx, t)
		if err != nil {
			return fmt.Sprintf("add_task failed: %v", err)
		}
		if stored == nil {
			return "already on the list (duplicate) — nothing added."
		}
		return fmt.Sprintf("added task %s: %q", stored.ID, oneLine(stored.Prompt))
	case "list_tasks":
		var kinds []scratchpad.TaskKind
		if k := str("kind"); k != "" {
			kinds = append(kinds, scratchpad.TaskKind(k))
		}
		tasks, err := c.tasks.List(ctx, kinds...)
		if err != nil {
			return fmt.Sprintf("list_tasks failed: %v", err)
		}
		if len(tasks) == 0 {
			return "the work-list is empty."
		}
		var b strings.Builder
		for _, t := range tasks {
			fmt.Fprintf(&b, "%s [%s] %s", t.ID, t.Kind, oneLine(t.Prompt))
			if t.Cron != "" {
				fmt.Fprintf(&b, " (cron %s)", t.Cron)
			}
			if t.RunAt > 0 {
				fmt.Fprintf(&b, " (at %s)", time.Unix(0, t.RunAt).Format(time.RFC3339))
			}
			if t.Target.Source != "" {
				fmt.Fprintf(&b, " → %s/%s", t.Target.Source, orSpawn(t.Target))
			}
			b.WriteString("\n")
		}
		return strings.TrimRight(b.String(), "\n")
	case "done_task":
		done := true
		if v, ok := args["done"].(bool); ok {
			done = v
		}
		ok, err := c.tasks.Complete(ctx, str("id"), done)
		if err != nil {
			return fmt.Sprintf("done_task failed: %v", err)
		}
		if !ok {
			return "no open task with that id (it may already be closed)."
		}
		if done {
			return "marked done."
		}
		return "marked failed."
	default:
		return fmt.Sprintf("unknown task tool %q.", name)
	}
}

func orSpawn(t scratchpad.Target) string {
	if t.Spawn {
		return "(new)"
	}
	return t.SessionID
}

func boolArg(args map[string]any, k string) bool {
	b, _ := args[k].(bool)
	return b
}

// oneLine clamps a prompt to a single line for list output.
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 80 {
		s = s[:77] + "…"
	}
	return s
}

func isTaskTool(name string) bool {
	switch name {
	case "add_task", "list_tasks", "done_task":
		return true
	}
	return false
}

// SetDecisionLog attaches the decision-log capture hook: called after every
// successful questionnaire submit, so both answer paths (concierge tool and
// PWA tap-to-answer) record identically.
func (c *Concierge) SetDecisionLog(fn func(q *source.Questionnaire, answers map[string]any, ctxLabel string)) *Concierge {
	c.logDecisions = fn
	return c
}
