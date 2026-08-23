package concierge

import (
	"context"
	"strings"
	"testing"

	"github.com/iodesystems/yscr/cue"
	"github.com/iodesystems/yscr/reports"
)

func TestRenderDiagram(t *testing.T) {
	c := New(nil, nil)
	c.WithReports(ReportState{
		CueTasks: func(ctx context.Context) ([]cue.Task, map[string]string, error) {
			return []cue.Task{{ID: "a", Prompt: "alpha"}, {ID: "b", Prompt: "beta", Deps: []string{"a"}}},
				map[string]string{"a": "done", "b": "pending"}, nil
		},
		Fleet: func(ctx context.Context) []reports.Session {
			return []reports.Session{{Source: "claude-code", ID: "s1", Title: "homelab", Status: "running"}}
		},
	})

	out := c.renderDiagram(context.Background(), "tasks")
	if !strings.Contains(out, "<diagram>") || !strings.Contains(out, "alpha") {
		t.Errorf("tasks diagram: %s", out)
	}
	out = c.renderDiagram(context.Background(), "fleet")
	if !strings.Contains(out, "homelab") {
		t.Errorf("fleet diagram: %s", out)
	}
	if got := c.renderDiagram(context.Background(), "bogus"); !strings.Contains(got, "unknown diagram kind") {
		t.Errorf("bogus kind: %s", got)
	}
}

func TestRenderDiagram_NoStore(t *testing.T) {
	c := New(nil, nil) // no WithReports → all state fns nil
	if got := c.renderDiagram(context.Background(), "tasks"); !strings.Contains(got, "durable store") {
		t.Errorf("no-store: %s", got)
	}
}

func TestWriteReportTool(t *testing.T) {
	c := New(nil, nil)
	home := t.TempDir()
	t.Setenv("HOME", home)
	out := c.reportDispatch(context.Background(), "write_report", map[string]any{"topic": "fleet status", "body": "# hi"})
	if !strings.Contains(out, ".yscr/reports") {
		t.Errorf("write_report: %s", out)
	}
	if got := c.reportDispatch(context.Background(), "write_report", map[string]any{"topic": "x"}); !strings.Contains(got, "no report body") {
		t.Errorf("empty body: %s", got)
	}
}

func TestIsReportTool(t *testing.T) {
	if !isReportTool("render_diagram") || !isReportTool("write_report") || isReportTool("post") {
		t.Error("isReportTool wrong")
	}
}
