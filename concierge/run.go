// Package concierge — run_command: the run & watch tool. The model proposes a
// command; the deterministic layer spawns a shell window, types the command in,
// and (foreground) waits for the shell to return to a prompt before reporting
// the tail of its output. Background commands are recorded as scratchpad
// command tasks and left running — the user watches them via Watch or asks.
package concierge

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/iodesystems/yscr/source"
)

// runCommandTimeout bounds a foreground wait for the shell to return to a
// prompt. A longer-running command should be sent in background mode instead.
const runCommandTimeout = 3 * time.Minute

// WithRun enables the run_command tool. `spawner` is the source that starts
// shell windows (the terminal pane adapter's source); `wait` blocks until the
// spawned session reports idle-at-prompt again, returning its final output tail.
func (c *Concierge) WithRun(spawner source.Source, wait func(ctx context.Context, ref source.SessionRef) (string, error)) *Concierge {
	c.runSpawner = spawner
	c.runWait = wait
	return c
}

var runToolDef = toolDef("run_command", "Run a terminal command in a NEW shell window. foreground=true waits for it to finish and returns the tail of its output; background=true starts it and returns immediately (the user can watch the pane). Use dir to set the working directory. For long-running work, prefer background.", objSchema(map[string]any{
	"command":    strProp("the command line to run, e.g. 'go test ./...'"),
	"dir":        strProp("working directory (absolute); empty = home"),
	"background": strProp("true = start and return immediately; false/omitted = wait for completion (bounded)"),
}, "command"))

// runCommand executes the tool call. The model picks; the spawn + poll is
// deterministic.
func (c *Concierge) runCommand(ctx context.Context, args map[string]any) string {
	cmd := strings.TrimSpace(fmt.Sprint(args["command"]))
	if cmd == "" {
		return "run_command needs a command."
	}
	if c.runSpawner == nil || c.runWait == nil {
		return "run_command is unavailable (no terminal source)."
	}
	bg, _ := args["background"].(bool)
	dir, _ := args["dir"].(string)
	spec := source.SpawnSpec{Title: firstWord(cmd), Dir: dir, Prompt: cmd}
	sp, ok := c.runSpawner.(source.Spawner)
	if !ok {
		return "run_command is unavailable (source cannot spawn)."
	}
	s, err := sp.Spawn(ctx, spec)
	if err != nil {
		return fmt.Sprintf("run_command failed to start a shell: %v", err)
	}
	if !bg {
		cctx, cancel := context.WithTimeout(ctx, runCommandTimeout)
		defer cancel()
		out, err := c.runWait(cctx, s)
		if err != nil {
			return fmt.Sprintf("command %q did not return to a prompt in time (it may still be running — the user can watch pane %s): %v", cmd, s.ID, err)
		}
		return fmt.Sprintf("command finished. Output tail:\n%s", out)
	}
	return fmt.Sprintf("command started in background (pane %s). The user can watch it or ask for its output.", s.ID)
}

func firstWord(s string) string {
	if i := strings.IndexAny(s, " \t"); i > 0 {
		return s[:i]
	}
	return s
}
