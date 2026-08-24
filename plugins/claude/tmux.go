package claude

import "context"

// Session is one Claude Code session as the source layer sees it: the adapter's
// own id plus where it lives (cwd, pid for the live-pane join). The generic
// pane source that used to define this type is gone; the claude adapter keeps a
// private copy because it is still wired in directly.
type Session struct {
	ID        string // claude sessionId
	Source    string // SourceID
	Cwd       string // working directory, if known
	Program   string // tmux pane_current_command when live ("claude"); "" if dormant
	Name      string // display name, if any
	Pid       int    // process pid, for the pid↔tty↔pane join (0 if unknown)
	UpdatedAt int64  // ns; for recency ordering
}

// Tmux is the pane-I/O plumbing the source lends adapters. It hides the exec
// seam so the adapter stays shell-free and tests inject a fake.
type Tmux interface {
	// Target resolves the tmux target to drive a session and whether it's live
	// (our own tracked window → the user's own pane via pid↔tty↔pane join → not
	// live). Returns a usable name even when not live.
	Target(ctx context.Context, s Session) (target string, live bool)
	// Capture returns the rendered pane viewport text (capture-pane -p).
	Capture(ctx context.Context, target string) (string, error)
	// Scrollback returns up to the last n lines of a pane's scrollback + viewport
	// (capture-pane -p -S -n). Meaningful only on the normal screen.
	Scrollback(ctx context.Context, target string, n int) (string, error)
	// SendKeys sends one send-keys invocation (arg tail after "-t target").
	SendKeys(ctx context.Context, target string, keys ...string) error
	// Launch starts a detached tmux window for session s in `dir` running argv,
	// returning the tmux target to drive it.
	Launch(ctx context.Context, s Session, dir string, argv []string) (target string, err error)
	// Pipe streams a pane's raw output (tmux pipe-pane) as byte chunks until the
	// returned stop func is called or ctx is cancelled.
	Pipe(ctx context.Context, target string) (<-chan []byte, func(), error)
}
