// Package claudecode is a source plugin whose sessions are Claude Code CLI
// processes running in detached tmux windows — the "claude-code-tmux virt"
// backend. The concierge spawns, observes, and posts to them by shelling out
// to tmux (new-session / send-keys / capture-pane), so a local Claude Code
// subscription becomes just another source alongside autowork + openai.
//
// It owns no protocol beyond tmux: the CLI in the pane is whatever Command
// points at (claude, or a wrapper / bridge). Interaction is asynchronous — a
// tmux pane is a live terminal, so Spawn/Post return immediately and State/
// Observe read the pane later.
package claudecode

import (
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/iodesystems/yscr/source"
)

const sourceID = "claude-code"

var (
	_ source.Source  = (*Plugin)(nil)
	_ source.Spawner = (*Plugin)(nil)
)

// Config tunes the plugin. Zero value is usable (tmux + claude + yscr-cc).
type Config struct {
	Tmux    string   // tmux binary; "" → "tmux"
	Command []string // the CLI to run in the pane; nil → ["claude"]
	Prefix  string   // tmux session-name prefix; "" → "yscr-cc"
}

// Plugin manages a set of tmux-hosted CLI sessions.
type Plugin struct {
	tmux    string
	command []string
	prefix  string
	now     func() int64
	// exec runs a command and returns combined output. Seam for tests.
	exec func(ctx context.Context, name string, args ...string) (string, error)

	mu   sync.Mutex
	seq  int
	sess map[string]*meta
}

type meta struct {
	id, tmux, title string
	created         int64
}

func New(cfg Config) *Plugin {
	p := &Plugin{
		tmux:    cfg.Tmux,
		command: cfg.Command,
		prefix:  cfg.Prefix,
		now:     func() int64 { return time.Now().UnixNano() },
		exec:    realExec,
		sess:    map[string]*meta{},
	}
	if p.tmux == "" {
		p.tmux = "tmux"
	}
	if len(p.command) == 0 {
		p.command = []string{"claude"}
	}
	if p.prefix == "" {
		p.prefix = "yscr-cc"
	}
	return p
}

func realExec(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(out), err
}

func (p *Plugin) tmuxCmd(ctx context.Context, args ...string) (string, error) {
	return p.exec(ctx, p.tmux, args...)
}

func (p *Plugin) ID() string { return sourceID }

func (p *Plugin) List(ctx context.Context) ([]source.SessionRef, error) {
	p.mu.Lock()
	metas := make([]*meta, 0, len(p.sess))
	for _, m := range p.sess {
		metas = append(metas, m)
	}
	p.mu.Unlock()

	var refs []source.SessionRef
	for _, m := range metas {
		// Drop sessions the user (or a crash) already closed in tmux.
		if _, err := p.tmuxCmd(ctx, "has-session", "-t", m.tmux); err != nil {
			p.mu.Lock()
			delete(p.sess, m.id)
			p.mu.Unlock()
			continue
		}
		refs = append(refs, source.SessionRef{Source: sourceID, ID: m.id, Title: m.title})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].ID < refs[j].ID })
	return refs, nil
}

func (p *Plugin) State(ctx context.Context, id string) (source.State, error) {
	p.mu.Lock()
	m := p.sess[id]
	p.mu.Unlock()
	if m == nil {
		return source.State{}, fmt.Errorf("claude-code: no session %q", id)
	}
	ref := source.SessionRef{Source: sourceID, ID: m.id, Title: m.title}
	pane, err := p.tmuxCmd(ctx, "capture-pane", "-t", m.tmux, "-p")
	if err != nil {
		// The tmux session is gone — the CLI exited.
		return source.State{Ref: ref, Status: source.StatusDone, Summary: "session ended"}, nil
	}
	return source.State{
		Ref:       ref,
		Status:    source.StatusRunning,
		Summary:   lastLines(pane, 3),
		UpdatedAt: p.now(),
	}, nil
}

func (p *Plugin) Post(ctx context.Context, id, message string) error {
	p.mu.Lock()
	m := p.sess[id]
	p.mu.Unlock()
	if m == nil {
		return fmt.Errorf("claude-code: no session %q", id)
	}
	// Type the message, then Enter (two send-keys so the literal text can't be
	// interpreted as a key name).
	if _, err := p.tmuxCmd(ctx, "send-keys", "-t", m.tmux, "-l", message); err != nil {
		return err
	}
	_, err := p.tmuxCmd(ctx, "send-keys", "-t", m.tmux, "Enter")
	return err
}

func (p *Plugin) Spawn(ctx context.Context, spec source.SpawnSpec) (source.SessionRef, error) {
	p.mu.Lock()
	p.seq++
	id := fmt.Sprintf("s%d", p.seq)
	name := fmt.Sprintf("%s-%s", p.prefix, id)
	p.sess[id] = &meta{id: id, tmux: name, title: spec.Title, created: p.now()}
	p.mu.Unlock()

	args := append([]string{"new-session", "-d", "-s", name}, p.command...)
	if _, err := p.tmuxCmd(ctx, args...); err != nil {
		p.mu.Lock()
		delete(p.sess, id)
		p.mu.Unlock()
		return source.SessionRef{}, fmt.Errorf("claude-code: start tmux session: %w", err)
	}
	if spec.Prompt != "" {
		// Best-effort initial prompt. The CLI may still be booting; the
		// concierge can always Post again.
		_ = p.postRaw(ctx, name, spec.Prompt)
	}
	return source.SessionRef{Source: sourceID, ID: id, Title: spec.Title}, nil
}

func (p *Plugin) postRaw(ctx context.Context, tmuxName, message string) error {
	if _, err := p.tmuxCmd(ctx, "send-keys", "-t", tmuxName, "-l", message); err != nil {
		return err
	}
	_, err := p.tmuxCmd(ctx, "send-keys", "-t", tmuxName, "Enter")
	return err
}

// Kill terminates a session's tmux window. Not part of source.Source — the
// service calls it on teardown.
func (p *Plugin) Kill(ctx context.Context, id string) error {
	p.mu.Lock()
	m := p.sess[id]
	if m != nil {
		delete(p.sess, id)
	}
	p.mu.Unlock()
	if m == nil {
		return nil
	}
	_, err := p.tmuxCmd(ctx, "kill-session", "-t", m.tmux)
	return err
}

// Observe emits a snapshot of the pane once, then closes. (Live streaming via
// periodic capture-pane diffs is a later enhancement.)
func (p *Plugin) Observe(ctx context.Context, id string) (<-chan source.Event, error) {
	p.mu.Lock()
	m := p.sess[id]
	p.mu.Unlock()
	if m == nil {
		return nil, fmt.Errorf("claude-code: no session %q", id)
	}
	ch := make(chan source.Event, 1)
	if pane, err := p.tmuxCmd(ctx, "capture-pane", "-t", m.tmux, "-p"); err == nil {
		ch <- source.Event{
			Ref:     source.SessionRef{Source: sourceID, ID: m.id, Title: m.title},
			Kind:    source.EventProgress,
			Content: lastLines(pane, 8),
			At:      p.now(),
		}
	}
	close(ch)
	return ch, nil
}

// lastLines returns the last n non-empty lines of a captured pane, joined.
func lastLines(pane string, n int) string {
	lines := strings.Split(strings.TrimRight(pane, "\n"), "\n")
	var kept []string
	for i := len(lines) - 1; i >= 0 && len(kept) < n; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			kept = append([]string{lines[i]}, kept...)
		}
	}
	return strings.Join(kept, "\n")
}
