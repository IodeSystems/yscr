package pane

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// tmuxDriver is the concrete Tmux the source lends adapters. It centralizes the
// exec seam and the pid↔tty↔pane join that adopts the user's own panes.
type tmuxDriver struct {
	bin    string
	prefix string // session-name prefix for windows we launch
	// exec runs a command → combined output. Seam for tests.
	exec  func(ctx context.Context, name string, args ...string) (string, error)
	ttyOf func(pid int) string // live pid → controlling tty ("" if dead); seam
	// pollInterval is how often Pipe reads new bytes from the pipe file. Seam for
	// tests (tighten it); 0 → pipePollDefault.
	pollInterval time.Duration
}

// pipePollDefault: the pipe file accumulates every byte, so this bounds latency,
// not completeness.
const pipePollDefault = 250 * time.Millisecond

func newTmux(bin, prefix string) *tmuxDriver {
	if bin == "" {
		bin = "tmux"
	}
	if prefix == "" {
		prefix = "yscr-cc"
	}
	return &tmuxDriver{
		bin: bin, prefix: prefix,
		exec:  realExec,
		ttyOf: defaultTTYOf,
	}
}

func realExec(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(out), err
}

func (d *tmuxDriver) run(ctx context.Context, args ...string) (string, error) {
	return d.exec(ctx, d.bin, args...)
}

// windowName maps a session id to the stable tmux window name we launch it as.
func (d *tmuxDriver) windowName(sid string) string { return d.prefix + "-" + sid }

func (d *tmuxDriver) Capture(ctx context.Context, target string) (string, error) {
	return d.run(ctx, "capture-pane", "-t", target, "-p")
}

func (d *tmuxDriver) Scrollback(ctx context.Context, target string, n int) (string, error) {
	if n <= 0 {
		n = 40
	}
	return d.run(ctx, "capture-pane", "-t", target, "-p", "-S", "-"+strconv.Itoa(n))
}

func (d *tmuxDriver) SendKeys(ctx context.Context, target string, keys ...string) error {
	_, err := d.run(ctx, append([]string{"send-keys", "-t", target}, keys...)...)
	return err
}

// Pipe streams a pane's raw output via `tmux pipe-pane 'cat >> tmpfile'`, then
// tails the file with an offset watermark. The file accumulates every byte, so
// nothing is lost between polls — the cadence only bounds latency. stop() ends
// the pipe (bare pipe-pane toggles it off), stops the tailer, and removes the
// file; it is safe to call once (the returned func guards re-entry via ctx).
func (d *tmuxDriver) Pipe(ctx context.Context, target string) (<-chan []byte, func(), error) {
	f, err := os.CreateTemp("", "yscr-pipe-*.raw")
	if err != nil {
		return nil, nil, err
	}
	path := f.Name()
	_ = f.Close()
	// Start the pipe. tmux runs the command via /bin/sh; the temp path is safe
	// (no spaces/metachars from CreateTemp).
	if _, err := d.run(ctx, "pipe-pane", "-t", target, "cat >> "+path); err != nil {
		_ = os.Remove(path)
		return nil, nil, err
	}

	out := make(chan []byte)
	ctx, cancel := context.WithCancel(ctx)
	var once sync.Once
	stop := func() { once.Do(cancel) }

	poll := d.pollInterval
	if poll <= 0 {
		poll = pipePollDefault
	}
	go func() {
		defer close(out)
		defer func() {
			// Toggle the pipe off (background ctx: the passed ctx is cancelled) and
			// clean up the temp file.
			_, _ = d.exec(context.Background(), d.bin, "pipe-pane", "-t", target)
			_ = os.Remove(path)
		}()
		rf, err := os.Open(path)
		if err != nil {
			return
		}
		defer rf.Close()
		ticker := time.NewTicker(poll)
		defer ticker.Stop()
		buf := make([]byte, 32*1024)
		drain := func() bool {
			for {
				n, _ := rf.Read(buf)
				if n == 0 {
					return true
				}
				chunk := append([]byte(nil), buf[:n]...)
				select {
				case out <- chunk:
				case <-ctx.Done():
					return false
				}
			}
		}
		for {
			select {
			case <-ctx.Done():
				drain() // final flush of whatever landed before stop
				return
			case <-ticker.C:
				if !drain() {
					return
				}
			}
		}
	}()
	return out, stop, nil
}

func (d *tmuxDriver) Launch(ctx context.Context, s Session, dir string, argv []string) (string, error) {
	name := d.windowName(s.ID)
	args := []string{"new-session", "-d", "-s", name, "-x", "220", "-y", "50"}
	if dir != "" {
		args = append(args, "-c", dir)
	}
	args = append(args, argv...)
	if _, err := d.run(ctx, args...); err != nil {
		return "", fmt.Errorf("pane: tmux new-session: %w", err)
	}
	return name, nil
}

// Target resolves how to drive a session: our own launched window → the user's
// own pane hosting it (exact pid→tty→pane join) → not live (returns the window
// name we'd use, so callers that only need a name have one).
func (d *tmuxDriver) Target(ctx context.Context, s Session) (string, bool) {
	own := d.windowName(s.ID)
	if _, err := d.run(ctx, "has-session", "-t", own); err == nil {
		return own, true
	}
	if tgt, ok := d.paneOf(ctx, s); ok {
		return tgt, true
	}
	return own, false
}

// paneOf finds the tmux pane hosting a session by joining the session's pid to a
// pane's controlling tty (#{pane_tty}). Exact — disambiguates multiple programs
// in the same cwd, unlike a cwd match. Needs Session.Pid, carried on live panes.
func (d *tmuxDriver) paneOf(ctx context.Context, s Session) (string, bool) {
	if s.Pid == 0 {
		return "", false
	}
	tty := d.ttyOf(s.Pid)
	if tty == "" {
		return "", false
	}
	tgt, ok := d.paneByTTY(ctx)[tty]
	return tgt, ok
}

// paneByTTY maps each tmux pane's controlling tty to its target address, across
// every session (including the user's own).
func (d *tmuxDriver) paneByTTY(ctx context.Context) map[string]string {
	out, err := d.run(ctx, "list-panes", "-a", "-F",
		"#{pane_tty}\t#{session_name}:#{window_index}.#{pane_index}")
	m := map[string]string{}
	if err != nil {
		return m
	}
	for _, ln := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if f := strings.SplitN(ln, "\t", 2); len(f) == 2 && f[0] != "" {
			m[f[0]] = f[1]
		}
	}
	return m
}

// scan lists every live tmux pane with the fields needed to classify + join it.
// The source routes each by Program to an adapter (LivePane defined in adapter.go).
func (d *tmuxDriver) scan(ctx context.Context) []LivePane {
	out, err := d.run(ctx, "list-panes", "-a", "-F",
		"#{pane_id}\t#{pane_pid}\t#{pane_current_command}\t#{pane_tty}\t#{alternate_on}")
	if err != nil {
		return nil
	}
	var panes []LivePane
	for _, ln := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		f := strings.SplitN(ln, "\t", 5)
		if len(f) != 5 {
			continue
		}
		panes = append(panes, LivePane{Target: f[0], Pid: atoi(f[1]), Program: f[2], TTY: f[3], Alt: f[4] == "1"})
	}
	return panes
}

// defaultTTYOf reads a live process's controlling tty from /proc (Linux). ""
// if the pid is gone or has no pts.
func defaultTTYOf(pid int) string {
	l, err := os.Readlink(fmt.Sprintf("/proc/%d/fd/0", pid))
	if err != nil || !strings.HasPrefix(l, "/dev/pts/") {
		return ""
	}
	return l
}

func atoi(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}
