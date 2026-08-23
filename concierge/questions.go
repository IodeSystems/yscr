// questions.go wires the open-questions queue into the concierge: a durable
// list of ambiguities parked while work that doesn't depend on them continues.
// The model proposes (ask_question / answer_question); the store persists; the
// PWA shows the list next to "Needs you" and a tap answers it — no LLM.
package concierge

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/iodesystems/agentkit/llm"

	"github.com/iodesystems/yscr/questions"
)

// WithQuestions attaches the open-questions tools. A nil store leaves the
// toolset unchanged (no DSN → no durable queue).
func (c *Concierge) WithQuestions(st questions.QuestionsStore) *Concierge {
	c.questions = st
	if st != nil {
		c.questionToolsOn = true
	}
	return c
}

var questionToolDefs = []llm.ToolDef{
	toolDef("ask_question", "Park an open question you need the user to answer before some work can proceed. Use when a task is ambiguous or blocked on a decision that is the USER's to make — then continue with whatever does NOT depend on the answer. Do not use for questions a tool can answer.", objSchema(map[string]any{
		"question": strProp("the question, one sentence"),
		"context":  strProp("why it matters / what is blocked on it (a few words)"),
		"task_id":  strProp("optional id of the task this blocks (from list_tasks or a goal plan)"),
	}, "question")),
	toolDef("list_questions", "List open questions awaiting the user. Use when the user asks what you need from them.", objSchema(map[string]any{})),
	toolDef("answer_question", "Record the user's answer to an open question (by its id from list_questions) and unblock the work it was holding. Use when the user answers one of your parked questions.", objSchema(map[string]any{
		"id":     strProp("the question id"),
		"answer": strProp("the user's answer, in their words"),
	}, "id", "answer")),
}

// ── dispatch ────────────────────────────────────────────────────────

func (c *Concierge) questionDispatch(ctx context.Context, name string, args map[string]any) string {
	if c.questions == nil {
		return "the open-questions queue is not configured (no durable store)."
	}
	str := func(k string) string { s, _ := args[k].(string); return s }

	switch name {
	case "ask_question":
		q := questions.Question{
			ID:        uuid.NewString(),
			Question:  str("question"),
			Context:   str("context"),
			TaskID:    str("task_id"),
			CreatedAt: time.Now().UnixNano(),
		}
		stored, err := c.questions.Add(ctx, q)
		if err != nil {
			return fmt.Sprintf("ask_question failed: %v", err)
		}
		if stored == nil {
			return "that question is already open — nothing added."
		}
		return fmt.Sprintf("parked question %s: %q. Continue with the work that does not depend on it; tell the user in one line what you need.", stored.ID, oneLine(stored.Question))
	case "list_questions":
		qs, err := c.questions.List(ctx)
		if err != nil {
			return fmt.Sprintf("list_questions failed: %v", err)
		}
		if len(qs) == 0 {
			return "no open questions — you have everything you need."
		}
		var b strings.Builder
		for _, q := range qs {
			fmt.Fprintf(&b, "%s %s", q.ID, oneLine(q.Question))
			if q.Context != "" {
				fmt.Fprintf(&b, " (%s)", oneLine(q.Context))
			}
			b.WriteString("\n")
		}
		return strings.TrimRight(b.String(), "\n")
	case "answer_question":
		ok, err := c.questions.Answer(ctx, str("id"), str("answer"))
		if err != nil {
			return fmt.Sprintf("answer_question failed: %v", err)
		}
		if !ok {
			return "no open question with that id (it may already be answered)."
		}
		return "recorded. Proceed with the work it was holding."
	default:
		return fmt.Sprintf("unknown question tool %q.", name)
	}
}

func isQuestionTool(name string) bool {
	switch name {
	case "ask_question", "list_questions", "answer_question":
		return true
	}
	return false
}
