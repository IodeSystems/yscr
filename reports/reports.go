// Package reports renders diagrams and detailed reports from validated state.
// The concierge's LLM picks the kind + scope; this package draws — no model in
// the rendering path (same line as cue/scratchpad: LLM proposes, deterministic
// layer produces).
package reports

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// DiagramKind is one of the renderers the concierge can ask for.
type DiagramKind string

const (
	DiagramTasks  DiagramKind = "tasks"  // cue-task dependency graph
	DiagramFleet  DiagramKind = "fleet"  // sessions grouped by source, colored by status
	DiagramStatus DiagramKind = "status" // scratchpad work list as a status board
)

// Node is one box in a diagram; Edge is a directed link (from → to).
type Node struct {
	ID    string
	Label string
	Sub   string // small caption line (status, dir, …)
	Color string // fill; "" = default
}

type Edge struct{ From, To string }

// Diagram is the intermediate form every renderer produces; SVG renders it.
type Diagram struct {
	Title string
	Nodes []Node
	Edges []Edge
}

// Task/Session are the minimal shapes renderers need — adapters copy from
// cue.Task / source.State so this package imports nothing from the repo.
type Task struct {
	ID       string
	Prompt   string
	Priority int
	Status   string
	Deps     []string
	Kind     string
	Cron     string
}

type Session struct {
	Source string
	ID     string
	Title  string
	Status string
}

// TaskGraph draws the cue-task dependency graph: one node per task (color by
// status), an edge from each dep to its dependent.
func TaskGraph(tasks []Task, statuses map[string]string) Diagram {
	colors := map[string]string{
		"done":     "#2e7d32",
		"inflight": "#1565c0",
		"pending":  "#9e9e9e",
	}
	d := Diagram{Title: "Task dependency graph"}
	byID := map[string]string{}
	for _, t := range tasks {
		st := statuses[t.ID]
		if st == "" {
			st = "pending"
		}
		byID[t.ID] = st
		d.Nodes = append(d.Nodes, Node{
			ID:    t.ID,
			Label: short(t.Prompt, 42),
			Sub:   fmt.Sprintf("%s · prio %d", st, t.Priority),
			Color: colors[st],
		})
	}
	for _, t := range tasks {
		for _, dep := range t.Deps {
			if byID[dep] != "" { // only draw edges to known tasks
				d.Edges = append(d.Edges, Edge{From: dep, To: t.ID})
			}
		}
	}
	return d
}

// FleetMap draws one node per live session, colored by status.
func FleetMap(sessions []Session) Diagram {
	colors := map[string]string{
		"running":       "#1565c0",
		"awaiting_user": "#ef6c00",
		"blocked":       "#c62828",
		"failed":        "#b71c1c",
		"done":          "#2e7d32",
		"idle":          "#9e9e9e",
	}
	d := Diagram{Title: "Fleet map"}
	for _, s := range sessions {
		id := s.Source + "/" + s.ID
		d.Nodes = append(d.Nodes, Node{
			ID:    id,
			Label: short(s.Title, 36),
			Sub:   fmt.Sprintf("%s · %s", s.Source, s.Status),
			Color: colors[s.Status],
		})
	}
	return d
}

// StatusBoard draws the scratchpad work list: one node per task, captioned by
// kind + status.
func StatusBoard(tasks []Task) Diagram {
	d := Diagram{Title: "Work list"}
	for _, t := range tasks {
		st := string(t.Status)
		if st == "" {
			st = "open"
		}
		sub := fmt.Sprintf("%s · %s", t.Kind, st)
		if t.Cron != "" {
			sub += " · ⟳" + t.Cron
		}
		d.Nodes = append(d.Nodes, Node{ID: t.ID, Label: short(t.Prompt, 42), Sub: sub})
	}
	return d
}

func short(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// ── SVG rendering ───────────────────────────────────────────────────

// SVG renders the diagram. Layout is a simple left-to-right layering: each
// node sits in a column equal to its longest dep-chain, so deps sit to the
// left of dependents; edges are straight lines with arrowheads. Good enough
// for ≤ ~20 nodes — the concierge's working set, not a graph-viz product.
func SVG(d Diagram) string {
	n := len(d.Nodes)
	if n == 0 {
		return svgDoc(80, 40, "<text x='10' y='25'>empty</text>")
	}

	const (
		w      = 230.0 // node width
		h      = 52.0  // node height
		gapX   = 90.0
		gapY   = 18.0
		margin = 20.0
	)

	pos := map[string]int{} // index in d.Nodes
	for i, nd := range d.Nodes {
		pos[nd.ID] = i
	}
	layer := map[string]int{}
	var depth func(id string, seen map[string]bool) int
	depth = func(id string, seen map[string]bool) int {
		if l, ok := layer[id]; ok {
			return l
		}
		if seen[id] {
			return 0 // cycle guard
		}
		seen[id] = true
		best := 0
		for _, e := range d.Edges {
			if e.To == id {
				if l := depth(e.From, seen) + 1; l > best {
					best = l
				}
			}
		}
		layer[id] = best
		return best
	}
	for _, nd := range d.Nodes {
		depth(nd.ID, map[string]bool{})
	}

	cols := map[int][]string{}
	maxCol := 0
	for _, nd := range d.Nodes {
		l := layer[nd.ID]
		cols[l] = append(cols[l], nd.ID)
		if l > maxCol {
			maxCol = l
		}
	}
	rowCount := 0
	for l := 0; l <= maxCol; l++ {
		sort.Strings(cols[l])
		if len(cols[l]) > rowCount {
			rowCount = len(cols[l])
		}
	}

	width := margin*2 + float64(maxCol+1)*w + float64(maxCol)*gapX
	height := margin*2 + float64(rowCount)*h + float64(max(0, rowCount-1))*gapY

	xs := map[string]float64{}
	ys := map[string]float64{}
	for l := 0; l <= maxCol; l++ {
		colH := float64(len(cols[l]))*h + float64(max(0, len(cols[l])-1))*gapY
		y0 := margin + (height-2*margin-colH)/2 // center the column vertically
		for i, id := range cols[l] {
			xs[id] = margin + float64(l)*(w+gapX)
			ys[id] = y0 + float64(i)*(h+gapY)
		}
	}

	var b strings.Builder
	b.WriteString("<defs><marker id='ar' markerWidth='8' markerHeight='8' refX='7' refY='3' orient='auto'><path d='M0,0 L7,3 L0,6 z' fill='#757575'/></marker></defs>\n")
	b.WriteString(fmt.Sprintf("<text x='%g' y='24' font-family='sans-serif' font-size='15' font-weight='bold'>%s</text>\n", margin, esc(d.Title)))

	for _, e := range d.Edges { // edges first (under the nodes)
		fx, fok := xs[e.From]
		tx, tok := xs[e.To]
		if !fok || !tok {
			continue
		}
		b.WriteString(fmt.Sprintf("<line x1='%g' y1='%g' x2='%g' y2='%g' stroke='#757575' marker-end='url(#ar)'/>\n",
			fx+w, ys[e.From]+h/2, tx, ys[e.To]+h/2))
	}

	for _, nd := range d.Nodes {
		fill := nd.Color
		if fill == "" {
			fill = "#616161"
		}
		x, y := xs[nd.ID], ys[nd.ID]
		b.WriteString(fmt.Sprintf("<g><rect x='%g' y='%g' width='%g' height='%g' rx='8' fill='%s'/>\n", x, y, w, h, fill))
		b.WriteString(fmt.Sprintf("<text x='%g' y='%g' font-family='sans-serif' font-size='12' fill='#fff'>%s</text>\n", x+10, y+20, esc(nd.Label)))
		if nd.Sub != "" {
			b.WriteString(fmt.Sprintf("<text x='%g' y='%g' font-family='sans-serif' font-size='10' fill='#e0e0e0'>%s</text>\n", x+10, y+38, esc(nd.Sub)))
		}
		b.WriteString("</g>\n")
	}

	return svgDoc(width, height, b.String())
}

func svgDoc(w, h float64, body string) string {
	return fmt.Sprintf(`<svg xmlns='http://www.w3.org/2000/svg' width='%d' height='%d' viewBox='0 0 %g %g'>%s</svg>`, int(w), int(h), w, h, body)
}

func esc(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "'", "&apos;")
	return r.Replace(s)
}

// ── reports on disk ─────────────────────────────────────────────────

// Dir is where reports are persisted (~/.yscr/reports).
func Dir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".yscr", "reports"), nil
}

// Write persists a markdown report and returns its path. The filename is
// timestamp + slug so reports are append-only artifacts.
func Write(topic, body string) (string, error) {
	d, err := Dir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(d, 0o755); err != nil {
		return "", err
	}
	name := fmt.Sprintf("%s-%s.md", time.Now().UTC().Format("20060102-150405"), slug(topic))
	p := filepath.Join(d, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		return "", err
	}
	return p, nil
}

func slug(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
		case b.Len() > 0 && b.String()[b.Len()-1] != '-':
			b.WriteByte('-')
		}
		if b.Len() > 40 {
			break
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "report"
	}
	return out
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
