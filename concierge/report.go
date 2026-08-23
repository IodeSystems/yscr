package concierge

import (
	"context"
	"fmt"

	"github.com/iodesystems/agentkit/llm"

	"github.com/iodesystems/yscr/cue"
	"github.com/iodesystems/yscr/reports"
)

// reportState is the durable state a renderer needs, injected by the service
// so the concierge package doesn't import store. Nil fns = that diagram kind
// is unavailable (no Postgres).
type ReportState struct {
	CueTasks func(ctx context.Context) ([]cue.Task, map[string]string, error) // tasks + status per id
	WorkList func(ctx context.Context) ([]reports.Task, error)                // scratchpad board
	Fleet    func(ctx context.Context) []reports.Session                      // live sessions
}

// WithReports attaches the render_diagram / write_report tools.
func (c *Concierge) WithReports(rs ReportState) *Concierge {
	c.report = rs
	return c
}

var reportToolDefs = []llm.ToolDef{
	toolDef("render_diagram", "Draw a diagram from the current state and return it as an SVG image for the user to see. kind: 'tasks' = the task dependency graph, 'fleet' = live sessions by status, 'status' = the work list (todos/schedules/commands). Call this when the user asks to SEE something — a graph, a map, a board.", objSchema(map[string]any{
		"kind": map[string]any{"type": "string", "enum": []string{"tasks", "fleet", "status"}, "description": "which diagram to draw"},
	}, "kind")),
	toolDef("write_report", "Write a detailed long-form report (markdown) and persist it as an artifact. Gather the material with fleet_status / pull_detail / read_history first, then pass the full markdown here. Returns the saved path; tell the user where it is.", objSchema(map[string]any{
		"topic": strProp("a short title for the report, e.g. 'fleet status' or 'homelab deep dive'"),
		"body":  strProp("the full markdown report"),
	}, "topic", "body")),
}

func (c *Concierge) reportDispatch(ctx context.Context, name string, args map[string]any) string {
	str := func(k string) string { s, _ := args[k].(string); return s }
	switch name {
	case "render_diagram":
		return c.renderDiagram(ctx, str("kind"))
	case "write_report":
		body, _ := args["body"].(string)
		if body == "" {
			return "no report body — gather the material first (fleet_status / pull_detail / read_history), then pass the markdown."
		}
		p, err := reports.Write(str("topic"), body)
		if err != nil {
			return fmt.Sprintf("write_report failed: %v", err)
		}
		return fmt.Sprintf("Report saved to %s", p)
	}
	return fmt.Sprintf("unknown report tool %q.", name)
}

func (c *Concierge) renderDiagram(ctx context.Context, kind string) string {
	rs := c.report
	var d reports.Diagram
	switch reports.DiagramKind(kind) {
	case reports.DiagramTasks:
		if rs.CueTasks == nil {
			return "the task graph needs the durable store (Postgres) — not configured."
		}
		tasks, statuses, err := rs.CueTasks(ctx)
		if err != nil {
			return fmt.Sprintf("task graph failed: %v", err)
		}
		rt := make([]reports.Task, 0, len(tasks))
		for _, t := range tasks {
			rt = append(rt, reports.Task{ID: t.ID, Prompt: t.Prompt, Priority: t.Priority, Status: statuses[t.ID], Deps: t.Deps})
		}
		d = reports.TaskGraph(rt, statuses)
	case reports.DiagramFleet:
		if rs.Fleet == nil {
			return "no live sessions to map."
		}
		d = reports.FleetMap(rs.Fleet(ctx))
	case reports.DiagramStatus:
		if rs.WorkList == nil {
			return "the work list needs the durable store (Postgres) — not configured."
		}
		tasks, err := rs.WorkList(ctx)
		if err != nil {
			return fmt.Sprintf("work list failed: %v", err)
		}
		d = reports.StatusBoard(tasks)
	default:
		return fmt.Sprintf("unknown diagram kind %q — use tasks, fleet, or status.", kind)
	}
	svg := reports.SVG(d)
	// The PWA renders <svg> inline in the chat bubble; keep the tool result
	// short so the model's reply stays conversational.
	return fmt.Sprintf("Diagram rendered (%d node(s)).\n<diagram>\n%s\n</diagram>", len(d.Nodes), svg)
}

// isReportTool reports whether name is one of the report tools.
func isReportTool(name string) bool {
	return name == "render_diagram" || name == "write_report"
}
