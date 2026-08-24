// Command tmuxanalyze — a read-only deep-dive over the live tmux tree using
// yscr's own Activity + terminal packages: scan, classify, and query each
// pane's history (head/tail/grep over lines; keyframes+diffs over TUI frames).
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/iodesystems/yscr/terminal"
	"github.com/iodesystems/yscr/tmux"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	a := tmux.New(tmux.Config{})
	panes, err := a.Scan(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "scan:", err)
		os.Exit(1)
	}
	fmt.Printf("== %d pane(s) ==\n", len(panes))
	for _, p := range panes {
		k := "lines"
		if p.Alt {
			k = "frames(TUI)"
		}
		s := a.SessionFor(ctx, p)
		fmt.Printf("  %-5s %-10s %-8s %-6s cwd=%-40s segs=%d\n",
			p.ID, p.Program, p.Target, k, short(p.Cwd), len(s.Segments()))
	}
	for _, p := range panes {
		s := a.SessionFor(ctx, p)
		fmt.Printf("\n== %s — %s @ %s ==\n", p.ID, p.Program, short(p.Cwd))
		out := s.QueryHistory(terminal.Query{Tail: 6})
		for _, ln := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
			fmt.Println("  | " + strings.TrimSpace(ln))
		}
	}
}

func short(d string) string {
	if len(d) > 40 {
		return "…" + d[len(d)-37:]
	}
	return d
}
