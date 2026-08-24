// Package tmux is the ACTIVITY that yields terminal sessions: it observes the
// live tmux tree, turns each PANE into a terminal.Session (fed with its
// output), can create windows/panes and send keys to any pane. It is not a
// source of its own — the service wraps it in one source.Source whose
// sessions ARE these terminal sessions.
//
// Feeding model: on every poll of a pane, the activity (1) checks whether the
// foreground program or screen kind changed → session.ProgramChanged (closes
// the open history segment, opens a new one); (2) for LINES panes, tails the
// scrollback with a line watermark and appends new lines; (3) for FRAMES
// panes, captures a keyframe on the throttle interval and appends it.
package tmux

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/iodesystems/yscr/source"
	"github.com/iodesystems/yscr/terminal"
)

// SourceID is the source id panes present as (SessionRef.Source).
const SourceID = "tmux"

// Activity drives tmux and yields terminal sessions.
type Activity struct {
	linePoll           time.Duration // LINES stream tick (default 250ms)
	bin              string
	exec             func(ctx context.Context, name string, args ...string) (string, error)
	now              func() int64
	keyframeInterval time.Duration // FRAMES capture cadence while streaming
	sessions         map[string]*terminal.Session
	wm               map[string]int    // per-pane line watermark for the feed
	streaming        map[string]bool   // panes with an active Stream loop
	mu               sync.Mutex
}

// Config tunes the activity. Empty = defaults.
type Config struct {
	Bin              string        // tmux binary (default "tmux")
	KeyframeInterval time.Duration // default 5s
}

func New(cfg Config) *Activity {
	bin := cfg.Bin
	if bin == "" {
		bin = "tmux"
	}
	ki := cfg.KeyframeInterval
	if ki <= 0 {
		ki = 5 * time.Second
	}
	return &Activity{
		linePoll:         250 * time.Millisecond,
		bin:              bin,
		exec:             realExec,
		now:              func() int64 { return time.Now().UnixNano() },
		keyframeInterval: ki,
		sessions:         map[string]*terminal.Session{},
		wm:               map[string]int{},
		streaming:        map[string]bool{},
	}
}

// ── the tmux tree ───────────────────────────────────────────────────

// Pane is one live pane — the surface a terminal session lives in. Its ID is
// its tmux pane id (%N), stable for the pane's lifetime.
type Pane struct {
	ID      string // %N
	Session string // tmux session name
	Window  int
	PaneIdx int
	Target  string // session:window.pane
	Pid     int    // pane_pid (the program process)
	Program string // pane_current_command
	TTY     string
	Cwd     string // resolved from the pid; "" if unknown
	Alt     bool   // alternate screen → frames history
	Active  bool
}

// Scan lists the full tmux tree.
func (a *Activity) Scan(ctx context.Context) ([]Pane, error) {
	out, err := a.run(ctx, "list-panes", "-a", "-F",
		"#{pane_id}\t#{session_name}\t#{window_index}\t#{pane_index}\t"+
			"#{pane_pid}\t#{pane_current_command}\t#{pane_tty}\t#{alternate_on}\t#{window_active}")
	if err != nil {
		return nil, fmt.Errorf("tmux: list-panes: %w", err)
	}
	var panes []Pane
	for _, ln := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		f := strings.SplitN(ln, "\t", 9)
		if len(f) != 9 {
			continue
		}
		p := Pane{
			ID: f[0], Session: f[1], Window: atoi(f[2]), PaneIdx: atoi(f[3]),
			Target:  f[1] + ":" + f[2] + "." + f[3],
			Pid:     atoi(f[4]), Program: f[5], TTY: f[6], Alt: f[7] == "1", Active: f[8] == "1",
		}
		p.Cwd = a.cwdOf(p.Pid)
		panes = append(panes, p)
	}
	return panes, nil
}

// PaneByID resolves a pane id to its current details (re-scans; the tree moves).
func (a *Activity) PaneByID(ctx context.Context, id string) (Pane, error) {
	for _, p := range mustScan(a, ctx) {
		if p.ID == id {
			return p, nil
		}
	}
	return Pane{}, fmt.Errorf("tmux: no pane %q", id)
}

// ── yielding terminal sessions ──────────────────────────────────────

// KindOf classifies a pane's current screen mode (live — it can flip).
func KindOf(p Pane) terminal.Kind {
	if p.Alt {
		return terminal.Frames
	}
	return terminal.Lines
}

// SessionFor returns the terminal session for a pane, creating it on first
// sight (seeded with whatever is already on screen so history isn't empty).
func (a *Activity) SessionFor(ctx context.Context, p Pane) *terminal.Session {
	a.mu.Lock()
	s, ok := a.sessions[p.ID]
	a.mu.Unlock()
	if ok {
		return s
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if s, ok = a.sessions[p.ID]; ok {
		return s
	}
	s = terminal.NewSession(p.ID, p.Program, KindOf(p), a.now())
	// Seed: what's already on screen becomes the first history content, and the
	// watermark starts at the seed size so the feed only appends NEW lines.
	if capr, err := a.run(ctx, "capture-pane", "-t", p.Target, "-p"); err == nil {
		for _, ln := range strings.Split(strings.TrimRight(capr, "\n"), "\n") {
			if KindOf(p) == terminal.Lines {
				s.AppendLine(ln)
				a.wm[p.ID]++
			} else {
				s.AppendFrame(a.now(), capr)
			}
		}
	}
	a.sessions[p.ID] = s
	return s
}

// Drop forgets a pane's session (its surface is gone).
func (a *Activity) Drop(id string) {
	a.mu.Lock()
	delete(a.sessions, id)
	delete(a.wm, id)
	a.mu.Unlock()
}

// ── feeding the sessions ────────────────────────────────────────────

// PollPane advances a pane's session: segment boundaries on program/kind
// change, new lines (watermark-tailed) for lines panes. Frames keyframes are
// captured by the stream loop instead (throttled), but a poll also records one
// so frame history has data even without an active stream.
func (a *Activity) PollPane(ctx context.Context, p Pane) {
	s := a.SessionFor(ctx, p)
	kind := KindOf(p)
	s.ProgramChanged(p.Program, kind, a.now())
	if kind == terminal.Lines {
		a.feedLines(ctx, p, s)
	} else if !a.isStreaming(p.ID) {
		s.AppendFrame(a.now(), a.viewport(ctx, p))
	}
}

func (a *Activity) isStreaming(id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.streaming[id]
}

// feedLines tails the scrollback with a per-pane line watermark.
func (a *Activity) feedLines(ctx context.Context, p Pane, s *terminal.Session) {
	cur, err := a.run(ctx, "capture-pane", "-t", p.Target, "-p", "-S", "-5000")
	if err != nil {
		return
	}
	lines := strings.Split(strings.TrimRight(cur, "\n"), "\n")
	a.mu.Lock()
	seen := a.wm[p.ID]
	a.mu.Unlock()
	if seen == 0 {
		seen = len(lines) // first feed after seeding: don't replay the seed
	}
	if len(lines) > seen {
		for _, ln := range lines[seen:] {
			s.AppendLine(ln)
		}
		seen = len(lines)
	}
	a.mu.Lock()
	a.wm[p.ID] = seen
	a.mu.Unlock()
}

// viewport captures the rendered frame of an alt-screen pane.
func (a *Activity) viewport(ctx context.Context, p Pane) string {
	out, _ := a.run(ctx, "capture-pane", "-t", p.Target, "-p")
	return out
}

// ── act: send keys / create ─────────────────────────────────────────

// SendKeys types into a pane: each line as literal text (send-keys -l) + Enter.
func (a *Activity) SendKeys(ctx context.Context, id, message string) error {
	p, err := a.PaneByID(ctx, id)
	if err != nil {
		return err
	}
	args := []string{"send-keys", "-t", p.Target}
	for _, line := range strings.Split(message, "\n") {
		args = append(args, "-l", line, "Enter")
	}
	_, err = a.run(ctx, args...)
	return err
}

// SpawnSpec is a create request: a new window (or pane split) running argv in
// dir. Session "" → the default session.
type SpawnSpec struct {
	Session string
	Dir     string
	Argv    []string
	Split   bool
}

// Spawn creates a new window (or split) and returns its pane id.
func (a *Activity) Spawn(ctx context.Context, spec SpawnSpec) (string, error) {
	// A new-window needs an existing session to attach to; create it first when
	// the named one is not there yet (tmux errors instead of creating).
	if !spec.Split && spec.Session != "" {
		if _, err := a.run(ctx, "has-session", "-t", spec.Session); err != nil {
			if out, e2 := a.run(ctx, "new-session", "-d", "-s", spec.Session); e2 != nil {
				return "", fmt.Errorf("tmux: create session %q: %w (out: %s)", spec.Session, e2, strings.TrimSpace(out))
			}
		}
	}
	var args []string
	if spec.Split {
		args = []string{"split-window", "-d"}
	} else {
		args = []string{"new-window", "-d"}
	}
	if spec.Session != "" {
		args = append(args, "-t", spec.Session+":")
	}
	if spec.Dir != "" {
		args = append(args, "-c", spec.Dir)
	}
	args = append(args, spec.Argv...)
	out, err := a.run(ctx, args...)
	if err != nil {
		return "", fmt.Errorf("tmux: spawn: %w (out: %s)", err, strings.TrimSpace(out))
	}
	want := base(spec.Argv[0])
	if panes, err := a.Scan(ctx); err == nil {
		for _, p := range panes {
			if spec.Session != "" && p.Session != spec.Session {
				continue
			}
			if p.Program == want {
				return p.ID, nil
			}
		}
	}
	return "", fmt.Errorf("tmux: spawned but could not resolve the new pane (Scan to find it)")
}

// ── stream: live lines or throttled keyframes ───────────────────────

// Stream emits a pane's output as source.Events until ctx is cancelled. LINES
// panes emit each new line; FRAMES panes capture a keyframe every
// KeyframeInterval and emit the DIFF vs the previous frame. It also FEEDS the
// pane's terminal session, so streaming doubles as history collection.
func (a *Activity) Stream(ctx context.Context, id string) (<-chan source.Event, error) {
	p, err := a.PaneByID(ctx, id)
	if err != nil {
		return nil, err
	}
	out := make(chan source.Event)
	ref := source.SessionRef{Source: SourceID, ID: p.ID}
	a.mu.Lock()
	a.streaming[p.ID] = true
	a.mu.Unlock()
	go func() {
		defer close(out)
		a.mu.Lock()
		delete(a.streaming, p.ID)
		a.mu.Unlock()
		s := a.SessionFor(ctx, p)
		if s == nil {
			return // pane vanished between resolution and seeding
		}
		emit := func(content string) bool {
			if strings.TrimSpace(content) == "" {
				return true
			}
			select {
			case out <- source.Event{Ref: ref, Kind: source.EventProgress, Content: content}:
				return true
			case <-ctx.Done():
				return false
			}
		}
		if KindOf(p) == terminal.Lines {
			a.streamLines(ctx, p, s, emit)
		} else {
			a.streamFrames(ctx, p, s, emit)
		}
	}()
	return out, nil
}

// streamLines tails with a watermark, feeding the session as it goes.
func (a *Activity) streamLines(ctx context.Context, p Pane, s *terminal.Session, emit func(string) bool) {
	// Baseline: the scrollback as it is NOW. The seed capture (-p) and the tail
	// capture (-S -5000) can differ in line count (history vs viewport), so the
	// watermark must come from THIS call — never from the seed's counter.
	cur, err := a.run(ctx, "capture-pane", "-t", p.Target, "-p", "-S", "-5000")
	if err != nil {
		a.Drop(p.ID)
		return
	}
	seen := len(strings.Split(strings.TrimRight(cur, "\n"), "\n"))
	a.mu.Lock()
	a.wm[p.ID] = seen
	a.mu.Unlock()
	ticker := time.NewTicker(a.linePoll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cur, err := a.run(ctx, "capture-pane", "-t", p.Target, "-p", "-S", "-5000")
			if err != nil {
				a.Drop(p.ID)
				return // pane gone
			}
			if cur == "" {
				continue // blank frame (screen cleared); wait for content
			}
			lines := strings.Split(strings.TrimRight(cur, "\n"), "\n")
			if len(lines) > seen {
				for _, ln := range lines[seen:] {
					s.AppendLine(ln)
					if !emit(ln) {
						return
					}
				}
				seen = len(lines)
			}
			a.mu.Lock()
			a.wm[p.ID] = seen
			a.mu.Unlock()
		}
	}
}

// streamFrames captures on the throttle, feeds the session, emits diffs.
func (a *Activity) streamFrames(ctx context.Context, p Pane, s *terminal.Session, emit func(string) bool) {
	// Baseline: capture NOW so the first emitted event is a real change, not a
	// replay of whatever was on screen when streaming started.
	prev := a.viewport(ctx, p)
	ticker := time.NewTicker(a.keyframeInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			frame := a.viewport(ctx, p)
			if frame == "" {
				a.Drop(p.ID)
				return
			}
			s.AppendFrame(a.now(), frame)
			if d := terminal.DiffFrames(prev, frame); d != "" {
				if !emit(d) {
					return
				}
			}
			prev = frame
		}
	}
}

// ── plumbing ────────────────────────────────────────────────────────

func (a *Activity) run(ctx context.Context, args ...string) (string, error) {
	return a.exec(ctx, a.bin, args...)
}

func realExec(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(out), err
}

// cwdOf resolves a pid's working directory from /proc (Linux). "" if unknown.
func (a *Activity) cwdOf(pid int) string {
	if pid == 0 {
		return ""
	}
	l, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid))
	if err != nil {
		return ""
	}
	return l
}

func mustScan(a *Activity, ctx context.Context) []Pane {
	ps, _ := a.Scan(ctx)
	return ps
}

func base(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}
