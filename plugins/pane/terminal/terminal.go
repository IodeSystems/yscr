// Package terminal is a stateless pane adapter for line-oriented programs —
// shells, builds, log tails: anything on the tmux NORMAL screen (alt=0). It has
// no session index; it adopts live panes the source scans (the Adopter seam) and
// reads their scrollback for history. It deliberately ignores alternate-screen
// TUIs (vim, htop, less, claude) — those have no scrollback and capture the
// keyboard, so scraping them is meaningless and driving them is unsafe. claude's
// own panes are handled by the claude adapter.
//
// This is the second adapter over plugins/pane, and its shape is the opposite of
// claude's: no Discover (nothing persists), history from the pane not a file,
// and no spawn/answer. It exists to prove the seam generalizes.
package terminal

import (
	"bytes"
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/iodesystems/yscr/plugins/pane"
	"github.com/iodesystems/yscr/source"
)

func nowNS() int64 { return time.Now().UnixNano() }

// SourceID is the source id terminal panes present as (SessionRef.Source).
const SourceID = "terminal"

var (
	_ pane.Adapter  = (*Adapter)(nil)
	_ pane.Adopter  = (*Adapter)(nil)
	_ pane.Streamer = (*Adapter)(nil)
)

// shells are the programs treated as "idle at a prompt" rather than running work.
var shells = map[string]bool{
	"bash": true, "fish": true, "zsh": true, "sh": true, "dash": true, "tcsh": true,
}

// Adapter implements pane.Adapter (+ Adopter) for line-oriented panes.
type Adapter struct {
	now func() int64
}

// Config is reserved for future tuning (allow/deny program lists). Empty today.
type Config struct{}

func New(_ Config) *Adapter {
	return &Adapter{now: nowNS}
}

func (a *Adapter) ID() string { return SourceID }

// Handles claims every program EXCEPT claude (owned by the claude adapter). The
// alt-screen gate that actually filters TUIs is applied in Adopt, where the pane
// state is known.
func (a *Adapter) Handles(program string) bool { return program != "claude" }

// Adopt materializes a Session from a live pane — but only a normal-screen one.
// An alt-screen pane (a full TUI) has no scrollback to read and captures input,
// so it's declined; the source then leaves it unmanaged, which is the honest
// outcome (the concierge can say "something's running there I can't read").
func (a *Adapter) Adopt(p pane.LivePane) (pane.Session, bool) {
	if p.Alt {
		return pane.Session{}, false
	}
	return pane.Session{
		Source: SourceID, ID: p.Target, Program: p.Program, Pid: p.Pid,
		Name: p.Program, UpdatedAt: a.now(),
	}, true
}

// Discover returns nothing — terminal panes don't persist; they exist only while
// live, and the source finds them by scanning + Adopt.
func (a *Adapter) Discover(context.Context) []pane.Session { return nil }

// State summarizes the pane from its current viewport. A shell program → idle
// (at a prompt); anything else running → running. No pending questions.
func (a *Adapter) State(ctx context.Context, s pane.Session, t pane.Tmux) (source.State, error) {
	ref := source.SessionRef{Source: SourceID, ID: s.ID, Title: s.Name, Dir: s.Cwd}
	status := source.StatusRunning
	if shells[s.Program] {
		status = source.StatusIdle
	}
	summary := ""
	if tgt, live := t.Target(ctx, s); live {
		capr, _ := t.Capture(ctx, tgt)
		summary = lastLines(capr, 3)
	}
	return source.State{Ref: ref, Status: status, Summary: summary, UpdatedAt: a.now()}, nil
}

// History reads the pane's scrollback — the width-invariant record for a
// line-oriented pane (n is a LINE count here, not turns; default via Scrollback).
func (a *Adapter) History(ctx context.Context, s pane.Session, n int, t pane.Tmux) (string, error) {
	tgt, live := t.Target(ctx, s)
	if !live {
		return "", source.ErrUnsupported // the pane is gone; nothing to read
	}
	out, err := t.Scrollback(ctx, tgt, n)
	if err != nil {
		return "", err
	}
	trimmed := strings.TrimRight(out, "\n")
	if strings.TrimSpace(trimmed) == "" {
		return "(no output)", nil
	}
	return trimmed, nil
}

// Post types a line into the pane (send-keys + Enter) — the concierge delivering
// input to a shell. It only drives a live pane; there's nothing to resume.
func (a *Adapter) Post(ctx context.Context, s pane.Session, message string, t pane.Tmux) error {
	tgt, live := t.Target(ctx, s)
	if !live {
		return source.ErrUnsupported
	}
	if err := t.SendKeys(ctx, tgt, "-l", message); err != nil {
		return err
	}
	return t.SendKeys(ctx, tgt, "Enter")
}

// Stream tails the pane's live output via pipe-pane, emitting one progress event
// per completed line (ANSI stripped, blank lines dropped). It runs until ctx is
// cancelled or the pane closes. This is the live-narration counterpart to the
// on-demand History snapshot.
func (a *Adapter) Stream(ctx context.Context, s pane.Session, t pane.Tmux) (<-chan source.Event, error) {
	ref := source.SessionRef{Source: SourceID, ID: s.ID, Title: s.Name, Dir: s.Cwd}
	tgt, live := t.Target(ctx, s)
	if !live {
		ch := make(chan source.Event)
		close(ch)
		return ch, nil
	}
	raw, stop, err := t.Pipe(ctx, tgt)
	if err != nil {
		return nil, err
	}
	out := make(chan source.Event)
	go func() {
		defer close(out)
		defer stop()
		emit := func(line string) {
			line = strings.TrimRight(stripANSI(line), " \t\r")
			if strings.TrimSpace(line) == "" {
				return
			}
			select {
			case out <- source.Event{Ref: ref, Kind: source.EventProgress, Content: line, At: a.now()}:
			case <-ctx.Done():
			}
		}
		var buf []byte
		for {
			select {
			case <-ctx.Done():
				return
			case chunk, ok := <-raw:
				if !ok { // pipe closed — flush a trailing partial line
					if len(buf) > 0 {
						emit(string(buf))
					}
					return
				}
				buf = append(buf, chunk...)
				for {
					i := bytes.IndexByte(buf, '\n')
					if i < 0 {
						break
					}
					emit(string(buf[:i]))
					buf = buf[i+1:]
				}
			}
		}
	}()
	return out, nil
}

// ansiRe strips the common terminal escape sequences (CSI, OSC, and lone
// two-byte escapes) so streamed lines are plain text.
var ansiRe = regexp.MustCompile("\x1b\\[[0-9;?]*[ -/]*[@-~]|\x1b\\][^\x07\x1b]*(\x07|\x1b\\\\)|\x1b[@-Z\\\\-_]")

func stripANSI(s string) string {
	s = ansiRe.ReplaceAllString(s, "")
	return strings.ReplaceAll(s, "\r", "")
}

// Spawn is unsupported — starting new work is claude's job, not a terminal's.
func (a *Adapter) Spawn(context.Context, source.SpawnSpec, pane.Tmux) (pane.Session, error) {
	return pane.Session{}, source.ErrUnsupported
}

// Act is unsupported — terminals surface no questionnaires.
func (a *Adapter) Act(context.Context, pane.Session, source.Action, pane.Tmux) (string, error) {
	return "", source.ErrUnsupported
}

// lastLines returns the last n non-empty lines of captured text, joined.
func lastLines(text string, n int) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	var kept []string
	for i := len(lines) - 1; i >= 0 && len(kept) < n; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			kept = append([]string{lines[i]}, kept...)
		}
	}
	return strings.Join(kept, "\n")
}
