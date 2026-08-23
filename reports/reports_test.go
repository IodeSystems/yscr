package reports

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTaskGraph(t *testing.T) {
	tasks := []Task{
		{ID: "a", Prompt: "alpha work"},
		{ID: "b", Prompt: "beta work", Deps: []string{"a"}},
		{ID: "c", Prompt: "gamma work", Deps: []string{"a", "missing"}},
	}
	d := TaskGraph(tasks, map[string]string{"a": "done", "b": "inflight"})
	if len(d.Nodes) != 3 {
		t.Fatalf("nodes = %d, want 3", len(d.Nodes))
	}
	// a→b and a→c; the edge to "missing" is dropped.
	if len(d.Edges) != 2 {
		t.Fatalf("edges = %d, want 2 (unknown dep dropped)", len(d.Edges))
	}
	svg := SVG(d)
	for _, want := range []string{`<svg`, "alpha work", "beta work", "inflight · prio 0", `marker-end='url(#ar)'`} {
		if !strings.Contains(svg, want) {
			t.Errorf("SVG missing %q", want)
		}
	}
}

func TestTaskGraph_Layering(t *testing.T) {
	tasks := []Task{
		{ID: "a", Prompt: "a"},
		{ID: "b", Prompt: "b", Deps: []string{"a"}},
		{ID: "c", Prompt: "c", Deps: []string{"b"}},
	}
	d := TaskGraph(tasks, nil)
	svg := SVG(d)
	// a must be drawn left of b, b left of c.
	// The node labeled "a" must be drawn before (left of) "b" and "c".
	xa := labelX(t, svg, ">a<")
	xb := labelX(t, svg, ">b<")
	xc := labelX(t, svg, ">c<")
	if !(xa < xb && xb < xc) {
		t.Errorf("layering wrong: a=%g b=%g c=%g", xa, xb, xc)
	}
}

// labelX finds the x attribute of the rect whose group contains the given
// node label text.
func labelX(t *testing.T, svg, label string) float64 {
	t.Helper()
	i := strings.Index(svg, label)
	if i < 0 {
		t.Fatalf("label %q not in svg", label)
	}
	gi := strings.LastIndex(svg[:i], "<g><rect x='")
	if gi < 0 {
		t.Fatalf("no group before %q", label)
	}
	j := gi + len("<g><rect x='")
	k := strings.IndexByte(svg[j:], 39)
	var x float64
	if _, err := fmt.Sscanf(svg[j:j+k], "%f", &x); err != nil {
		t.Fatalf("parse x %q: %v", svg[j:j+k], err)
	}
	return x
}

func TestFleetMap(t *testing.T) {
	d := FleetMap([]Session{
		{Source: "claude-code", ID: "s1", Title: "homelab <prod>", Status: "awaiting_user"},
		{Source: "terminal", ID: "sh-abc", Title: "shell", Status: "running"},
	})
	if len(d.Nodes) != 2 {
		t.Fatalf("nodes = %d, want 2", len(d.Nodes))
	}
	svg := SVG(d)
	if !strings.Contains(svg, "homelab &lt;prod&gt;") {
		t.Errorf("title not escaped: %s", svg)
	}
}

func TestStatusBoard(t *testing.T) {
	d := StatusBoard([]Task{{ID: "t1", Prompt: "water plants", Kind: "todo", Cron: "0 9 * * *"}})
	if len(d.Nodes) != 1 || !strings.Contains(d.Nodes[0].Sub, "⟳0 9 * * *") {
		t.Fatalf("board node = %+v", d.Nodes)
	}
}

func TestSVG_Empty(t *testing.T) {
	if s := SVG(Diagram{}); !strings.Contains(s, "empty") {
		t.Errorf("empty diagram: %s", s)
	}
}

func TestWrite(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	p, err := Write("Fleet Status Report!", "# hi\n")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".yscr", "reports")
	if !strings.HasPrefix(p, want) {
		t.Fatalf("path %q not under %q", p, want)
	}
	base := filepath.Base(p)
	if !strings.Contains(base, "fleet-status-report") || !strings.HasSuffix(base, ".md") {
		t.Errorf("slug wrong: %s", base)
	}
	b, err := os.ReadFile(p)
	if err != nil || string(b) != "# hi\n" {
		t.Fatalf("read back %q err=%v", b, err)
	}
}

func TestShort(t *testing.T) {
	if got := short("hello", 10); got != "hello" {
		t.Errorf("short = %q", got)
	}
	if got := short(strings.Repeat("x", 50), 10); !strings.HasSuffix(got, "…") || len([]rune(got)) != 10 {
		t.Errorf("short long = %q", got)
	}
}
