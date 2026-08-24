// decisions.go wires conversational recall of the decision log into the
// concierge: the user can ask "why did you pick EU?" / "what have I decided?"
// and get the recorded answers with their provenance. Read-only — capture and
// auto-resolve live in the service (both answer paths).
package concierge

import (
	"context"
	"fmt"
	"strings"

	"github.com/iodesystems/agentkit/llm"

	"github.com/iodesystems/yscr/decisions"
)

// WithDecisions attaches the list_decisions tool. A nil store leaves the
// toolset unchanged (no durable store → nothing to recall).
func (c *Concierge) WithDecisions(st decisions.Store) *Concierge {
	c.decisions = st
	if st != nil {
		c.decisionToolsOn = true
	}
	return c
}

var decisionToolDefs = []llm.ToolDef{
	toolDef("list_decisions", "Recall the user's recorded decisions from past questions — what was chosen, when, and in which context. Use when the user asks why something was decided a certain way, or before re-asking a question that may already be answered.", objSchema(map[string]any{
		"filter": strProp("optional: only decisions whose question matches this substring"),
	})),
}

func (c *Concierge) decisionDispatch(ctx context.Context, name string, args map[string]any) string {
	if c.decisions == nil {
		return "the decision log is not configured (no durable store)."
	}
	switch name {
	case "list_decisions":
		raw, _ := args["filter"].(string)
	f := strings.ToLower(strings.TrimSpace(raw))
		ds, err := c.decisions.List(decisions.StatusOpen)
		if err != nil {
			return fmt.Sprintf("list_decisions failed: %v", err)
		}
		var b strings.Builder
		n := 0
		for _, d := range ds {
			if f != "" && !strings.Contains(strings.ToLower(d.Question), f) {
				continue
			}
			fmt.Fprintf(&b, "%s: %q = %s", d.CreatedAt.Format("2006-01-02"), d.Question, d.Answer)
			if d.Context != "" {
				fmt.Fprintf(&b, " (%s)", oneLine(d.Context))
			}
			b.WriteString("\n")
			n++
			if n >= 25 {
				break
			}
		}
		if n == 0 {
			if f != "" {
				return fmt.Sprintf("no recorded decisions matching %q.", f)
			}
			return "no recorded decisions."
		}
		return strings.TrimRight(b.String(), "\n")
	default:
		return fmt.Sprintf("unknown decision tool %q.", name)
	}
}

func isDecisionTool(name string) bool { return name == "list_decisions" }
