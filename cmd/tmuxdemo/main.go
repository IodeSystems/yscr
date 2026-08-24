package main

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/iodesystems/yscr/terminal"
	tmux "github.com/iodesystems/yscr/tmux"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	a := tmux.New(tmux.Config{})

	// 1. SCAN: the live tmux tree as panes.
	panes, err := a.Scan(ctx)
	if err != nil {
		fmt.Println("scan:", err)
		return
	}
	fmt.Printf("== SCAN — %d live pane(s) ==\n", len(panes))
	for _, p := range panes {
		fmt.Printf("  %-4s %-12s %-8s %-6s alt=%-1v cwd=%s\n",
			p.ID, p.Program, p.Target, p.Session, p.Alt, short(p.Cwd))
	}

	// 2. SPAWN: open a demo window running a shell that prints lines.
	id, err := a.Spawn(ctx, tmux.SpawnSpec{Session: "yscr-demo", Dir: "/tmp",
		Argv: []string{"sh", "-c", "for i in 1 2 3 4 5; do echo build-step-$i ok; sleep 0.7; done; echo DONE; exec sh"}})
	if err != nil {
		fmt.Println("spawn:", err)
		return
	}
	fmt.Printf("\n== SPAWN — new pane %s ==\n", id)

	// 3. STREAM: follow only the NEW lines as they appear (watermark tail).
	time.Sleep(400 * time.Millisecond) // let the pane come up
	ch, err := a.Stream(ctx, id)
	if err != nil {
		fmt.Println("stream:", err)
		return
	}
	fmt.Println("\n== STREAM — new lines as they appear ==")
	deadline := time.After(6 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				goto history
			}
			fmt.Printf("  [%s] %s\n", ev.Kind, ev.Content)
		case <-deadline:
			goto history
		}
	}

history:
	// 4. HISTORY: the pane's session, segmented by program; query it.
	p2, _ := a.PaneByID(ctx, id)
	s := a.SessionFor(ctx, p2)
	fmt.Println("\n== HISTORY — segments ==")
	for i, seg := range s.Segments() {
		fmt.Printf("  seg %d: program=%-6s kind=%-6s lines=%d\n", i, seg.Program, seg.Kind, len(seg.Lines))
	}
	fmt.Println("== HISTORY — tail 4 ==")
	fmt.Print(indent(s.QueryHistory(terminal.Query{Tail: 4})))
	fmt.Println("== HISTORY — grep 'step-3' ==")
	fmt.Print(indent(s.QueryHistory(terminal.Query{Grep: "step-3"})))

	// 5. SEND-KEYS: type a command into the pane, read the answer back.
	if err := a.SendKeys(ctx, id, "echo typed-by-yscr"); err != nil {
		fmt.Println("send-keys:", err)
		return
	}
	time.Sleep(700 * time.Millisecond)
	a.PollPane(ctx, p2)
	s = a.SessionFor(ctx, p2)
	fmt.Println("== SEND-KEYS — echo typed-by-yscr; pane now shows ==")
	fmt.Print(indent(s.QueryHistory(terminal.Query{Tail: 2})))

	// cleanup: the demo session is ours.
	exec.CommandContext(ctx, "tmux", "kill-session", "-t", "yscr-demo").Run()
	fmt.Println("\n== CLEANUP — killed yscr-demo session ==")
}

func short(p string) string {
	if len(p) > 24 {
		return "…" + p[len(p)-23:]
	}
	return p
}
func indent(s string) string { return "\n" + strings.ReplaceAll(s, "\n", "\n") + "\n" }

var _ = exec.Command
