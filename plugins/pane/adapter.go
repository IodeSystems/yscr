// Package pane is a generic tmux-pane source. It owns the tmux plumbing — pane
// scanning, the pid↔tty↔pane join, capture-pane, send-keys, launch — and routes
// each pane to a program Adapter that supplies the semantics (history, question
// detection, answering, spawn/resume). A program is pluggable: implement Adapter
// and register it. claude is the first adapter (plugins/pane/claude).
//
// Two axes meet here. A live pane is classified by its foreground program
// (tmux #{pane_current_command}) → Adapter.Handles. Beyond live panes, an adapter
// contributes persistent sessions via Discover (claude reads ~/.claude/sessions;
// a stateless adapter returns nil). The source unions the two.
package pane

import (
	"context"

	"github.com/iodesystems/yscr/source"
)

// Session is one addressable session an adapter owns — a live pane, a persistent
// (resumable) session, or both. The pane source assembles these from live-pane
// classification and each adapter's Discover, then routes operations back to the
// owning adapter.
type Session struct {
	ID        string // adapter-local session id (claude sessionId, …)
	Source    string // owning adapter id → SessionRef.Source
	Cwd       string // working directory, if known
	Program   string // tmux pane_current_command when live ("claude"); "" if dormant
	Name      string // adapter's display name, if any
	Pid       int    // process pid, for the pid↔tty↔pane join (0 if unknown)
	UpdatedAt int64  // ns; for recency ordering
}

// Adapter understands one kind of program running in a tmux pane. The pane
// source lends it a Tmux handle for any pane I/O so the adapter never shells out
// directly (keeps the exec seam in one place, test-fakeable).
type Adapter interface {
	// ID is the source id sessions from this adapter present as
	// (SessionRef.Source, e.g. "claude-code").
	ID() string

	// Handles reports whether a live pane running `program` (pane_current_command)
	// belongs to this adapter.
	Handles(program string) bool

	// Discover returns persistent sessions this adapter knows beyond live panes
	// (claude reads its session index). Stateless adapters return nil.
	Discover(ctx context.Context) []Session

	// State returns the digest rollup for one session. `tmux` gives pane access;
	// the source has already resolved the live target (target, live) via the join.
	State(ctx context.Context, s Session, t Tmux) (source.State, error)

	// History projects a session's recent conversation/output to compact
	// width-invariant text — claude reads its JSONL transcript (ignores tmux); a
	// terminal reads pane scrollback via tmux. n<=0 → adapter default.
	History(ctx context.Context, s Session, n int, t Tmux) (string, error)

	// Post injects a user message into a session, resuming it if dormant. `tmux`
	// carries the launch/send plumbing.
	Post(ctx context.Context, s Session, message string, t Tmux) error

	// Spawn starts new work, returning the new session. Adapters that can't spawn
	// return source.ErrUnsupported.
	Spawn(ctx context.Context, spec source.SpawnSpec, t Tmux) (Session, error)

	// Act performs a mediated action (answering a questionnaire by driving the
	// live TUI). Adapters without actions return source.ErrUnsupported.
	Act(ctx context.Context, s Session, action source.Action, t Tmux) (string, error)
}

// Streamer is the optional capability to stream a session's output as live
// events until ctx is cancelled — the continuous alternative to the one-shot
// Observe summary. The terminal adapter streams pane output via tmux pipe-pane;
// a stateful adapter could tail its transcript instead. Source.Observe delegates
// here when the adapter implements it.
type Streamer interface {
	Stream(ctx context.Context, s Session, t Tmux) (<-chan source.Event, error)
}

// Adopter is the optional capability to materialize a Session from a live pane
// the adapter Handles but does NOT persist — a stateless program (a shell, a
// build, a log tail). Stateful adapters (claude) enumerate via Discover instead
// and don't implement this; the source's pane scan then skips them.
type Adopter interface {
	Adopt(p LivePane) (Session, bool)
}

// LivePane is one live tmux pane from a full scan, handed to Adopter.Adopt.
type LivePane struct {
	Target  string // pane id (%N) — stable for the pane's lifetime
	Pid     int
	Program string // pane_current_command
	TTY     string
	Alt     bool // on the alternate screen (a full-screen TUI: vim, htop, claude)
}

// Tmux is the pane-I/O plumbing the source lends adapters. It hides the exec
// seam so adapters stay shell-free and tests inject a fake.
type Tmux interface {
	// Target resolves the tmux target to drive a session and whether it's live
	// (our own tracked window → the user's own pane via pid↔tty↔pane join → not
	// live). Returns a usable name even when not live.
	Target(ctx context.Context, s Session) (target string, live bool)
	// Capture returns the rendered pane viewport text (capture-pane -p).
	Capture(ctx context.Context, target string) (string, error)
	// Scrollback returns up to the last n lines of a pane's scrollback + viewport
	// (capture-pane -p -S -n). Meaningful only on the normal screen (an alt-screen
	// TUI has no scrollback); the terminal adapter gates on LivePane.Alt.
	Scrollback(ctx context.Context, target string, n int) (string, error)
	// SendKeys sends one send-keys invocation (arg tail after "-t target").
	SendKeys(ctx context.Context, target string, keys ...string) error
	// Launch starts a detached tmux window for session s in `dir` running argv,
	// returning the tmux target to drive it (the window name; Target's has-session
	// finds it thereafter, so no separate tracking is needed).
	Launch(ctx context.Context, s Session, dir string, argv []string) (target string, err error)
	// Pipe streams a pane's raw output (tmux pipe-pane) as byte chunks until the
	// returned stop func is called or ctx is cancelled. Chunks are raw terminal
	// bytes (may contain ANSI); the adapter projects them. stop ends the pipe and
	// releases resources; it is safe to call once.
	Pipe(ctx context.Context, target string) (<-chan []byte, func(), error)
}
