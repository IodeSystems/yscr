// Package claudecode is a source plugin over the Claude Code CLI (`claude`)
// running in detached tmux windows — the "claude-code-tmux" backend. It reads
// Claude's own session metadata from the user's home dir (~/.claude) so it can:
//
//   - LIST + RESUME existing sessions: ~/.claude/sessions/*.json is Claude's
//     live/recent session index (sessionId, cwd, status, updatedAt); resume is
//     `claude --resume <sessionId>` in that session's cwd.
//   - LAUNCH new sessions in a directory: `claude --session-id <uuid>` under a
//     chosen working dir (tmux -c <dir>).
//
// Interaction is asynchronous — a tmux pane is a live terminal, so Spawn/Post
// return immediately; State/Observe read the pane or the JSONL transcript
// (~/.claude/projects/<enc-cwd>/<sid>.jsonl). Mechanics mirror the ccoa bridge.
package claudecode

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

// Config tunes the plugin. Zero value is usable (tmux + claude + yscr-cc + 25).
type Config struct {
	Tmux    string   // tmux binary; "" → "tmux"
	Command []string // the CLI + fixed args; nil → ["claude"]
	Prefix  string   // tmux session-name prefix; "" → "yscr-cc"
	Home    string   // Claude config dir; "" → $CLAUDE_CONFIG_DIR or ~/.claude
	Limit   int       // max sessions List returns (most-recent); 0 → 25
}

type Plugin struct {
	tmux    string
	command []string
	prefix  string
	home    string
	limit   int
	now     func() int64
	// exec runs a command and returns combined output. Seam for tests.
	exec  func(ctx context.Context, name string, args ...string) (string, error)
	newID func() string

	mu   sync.Mutex
	live map[string]liveSess // sid → the tmux window we drive it in
}

type liveSess struct {
	tmux string
	cwd  string
}

func New(cfg Config) *Plugin {
	p := &Plugin{
		tmux: cfg.Tmux, command: cfg.Command, prefix: cfg.Prefix, home: cfg.Home, limit: cfg.Limit,
		now:   func() int64 { return time.Now().UnixNano() },
		exec:  realExec,
		newID: newUUID,
		live:  map[string]liveSess{},
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
	if p.home == "" {
		if v := os.Getenv("CLAUDE_CONFIG_DIR"); v != "" {
			p.home = v
		} else {
			h, _ := os.UserHomeDir()
			p.home = filepath.Join(h, ".claude")
		}
	}
	if p.limit <= 0 {
		p.limit = 25
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

// ── Claude session metadata (~/.claude) ─────────────────────────────

// sessionMeta is one entry of Claude's session index (~/.claude/sessions/*.json).
type sessionMeta struct {
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
	Status    string `json:"status"`
	Kind      string `json:"kind"`
	UpdatedAt int64  `json:"updatedAt"`
	StartedAt int64  `json:"startedAt"`
}

// readIndex loads the session index, keyed by sessionId (newest wins).
func (p *Plugin) readIndex() map[string]sessionMeta {
	out := map[string]sessionMeta{}
	files, _ := filepath.Glob(filepath.Join(p.home, "sessions", "*.json"))
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var m sessionMeta
		if json.Unmarshal(b, &m) != nil || m.SessionID == "" {
			continue
		}
		if prev, ok := out[m.SessionID]; !ok || m.UpdatedAt >= prev.UpdatedAt {
			out[m.SessionID] = m
		}
	}
	return out
}

// projectDir is where Claude stores a workdir's transcripts.
func (p *Plugin) projectDir(cwd string) string {
	repl := strings.NewReplacer("/", "-", ".", "-")
	return filepath.Join(p.home, "projects", repl.Replace(cwd))
}

func (p *Plugin) transcriptPath(cwd, sid string) string {
	return filepath.Join(p.projectDir(cwd), sid+".jsonl")
}

// cwdOf resolves a session's working dir from our live map or the index.
func (p *Plugin) cwdOf(sid string) string {
	p.mu.Lock()
	if ls, ok := p.live[sid]; ok && ls.cwd != "" {
		p.mu.Unlock()
		return ls.cwd
	}
	p.mu.Unlock()
	return p.readIndex()[sid].Cwd
}

// ── source.Source ───────────────────────────────────────────────────

func (p *Plugin) List(_ context.Context) ([]source.SessionRef, error) {
	metas := p.readIndex()
	// Include anything we're actively driving (may pre-date its index entry).
	p.mu.Lock()
	for sid, ls := range p.live {
		if _, ok := metas[sid]; !ok {
			metas[sid] = sessionMeta{SessionID: sid, Cwd: ls.cwd, UpdatedAt: p.now()}
		}
	}
	p.mu.Unlock()

	list := make([]sessionMeta, 0, len(metas))
	for _, m := range metas {
		list = append(list, m)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].UpdatedAt > list[j].UpdatedAt })
	if len(list) > p.limit {
		list = list[:p.limit]
	}
	refs := make([]source.SessionRef, 0, len(list))
	for _, m := range list {
		refs = append(refs, source.SessionRef{Source: sourceID, ID: m.SessionID, Title: title(m.Cwd)})
	}
	return refs, nil
}

func (p *Plugin) State(ctx context.Context, sid string) (source.State, error) {
	cwd := p.cwdOf(sid)
	ref := source.SessionRef{Source: sourceID, ID: sid, Title: title(cwd)}

	// Running in our tmux? → live pane.
	name := p.tmuxName(sid)
	if _, err := p.tmuxCmd(ctx, "has-session", "-t", name); err == nil {
		pane, _ := p.tmuxCmd(ctx, "capture-pane", "-t", name, "-p")
		return source.State{Ref: ref, Status: source.StatusRunning, Summary: lastLines(pane, 3), UpdatedAt: p.now()}, nil
	}

	// Dormant: read the transcript tail. Resumable via Post.
	if cwd == "" {
		return source.State{}, fmt.Errorf("claude-code: unknown session %q", sid)
	}
	summary := lastAssistantText(p.transcriptPath(cwd, sid))
	if summary == "" {
		summary = "(no transcript)"
	}
	return source.State{Ref: ref, Status: source.StatusIdle, Summary: summary, UpdatedAt: p.now()}, nil
}

func (p *Plugin) Post(ctx context.Context, sid, message string) error {
	name := p.tmuxName(sid)
	if _, err := p.tmuxCmd(ctx, "has-session", "-t", name); err != nil {
		// Not running under us — resume the dormant session in its cwd.
		cwd := p.cwdOf(sid)
		if cwd == "" {
			return fmt.Errorf("claude-code: cannot resume unknown session %q", sid)
		}
		if err := p.launch(ctx, name, cwd, append(sliceOf(p.command), "--resume", sid)); err != nil {
			return err
		}
		p.track(sid, name, cwd)
	}
	return p.send(ctx, name, message)
}

// Spawn starts a NEW Claude session in spec.Dir (default: cwd).
func (p *Plugin) Spawn(ctx context.Context, spec source.SpawnSpec) (source.SessionRef, error) {
	sid := p.newID()
	name := p.tmuxName(sid)
	dir := spec.Dir
	if dir == "" {
		dir, _ = os.Getwd()
	}
	if err := p.launch(ctx, name, dir, append(sliceOf(p.command), "--session-id", sid)); err != nil {
		return source.SessionRef{}, err
	}
	p.track(sid, name, dir)
	if spec.Prompt != "" {
		_ = p.send(ctx, name, spec.Prompt)
	}
	t := spec.Title
	if t == "" {
		t = title(dir)
	}
	return source.SessionRef{Source: sourceID, ID: sid, Title: t}, nil
}

// Observe emits the latest assistant reply once, then closes.
func (p *Plugin) Observe(ctx context.Context, sid string) (<-chan source.Event, error) {
	cwd := p.cwdOf(sid)
	ref := source.SessionRef{Source: sourceID, ID: sid, Title: title(cwd)}
	ch := make(chan source.Event, 1)
	var content string
	name := p.tmuxName(sid)
	if _, err := p.tmuxCmd(ctx, "has-session", "-t", name); err == nil {
		pane, _ := p.tmuxCmd(ctx, "capture-pane", "-t", name, "-p")
		content = lastLines(pane, 8)
	} else if cwd != "" {
		content = lastAssistantText(p.transcriptPath(cwd, sid))
	}
	if content != "" {
		ch <- source.Event{Ref: ref, Kind: source.EventProgress, Content: content, At: p.now()}
	}
	close(ch)
	return ch, nil
}

// Kill terminates a session's tmux window (not part of source.Source).
func (p *Plugin) Kill(ctx context.Context, sid string) error {
	name := p.tmuxName(sid)
	p.mu.Lock()
	delete(p.live, sid)
	p.mu.Unlock()
	_, err := p.tmuxCmd(ctx, "kill-session", "-t", name)
	return err
}

// ── tmux + helpers ──────────────────────────────────────────────────

// tmuxName maps a Claude session id to a stable tmux window name.
func (p *Plugin) tmuxName(sid string) string { return p.prefix + "-" + sid }

// launch starts a detached tmux window running `program...` in dir.
func (p *Plugin) launch(ctx context.Context, name, dir string, program []string) error {
	args := []string{"new-session", "-d", "-s", name, "-x", "220", "-y", "50"}
	if dir != "" {
		args = append(args, "-c", dir)
	}
	args = append(args, program...)
	if _, err := p.tmuxCmd(ctx, args...); err != nil {
		return fmt.Errorf("claude-code: tmux new-session: %w", err)
	}
	return nil
}

func (p *Plugin) send(ctx context.Context, name, message string) error {
	if _, err := p.tmuxCmd(ctx, "send-keys", "-t", name, "-l", message); err != nil {
		return err
	}
	_, err := p.tmuxCmd(ctx, "send-keys", "-t", name, "Enter")
	return err
}

func (p *Plugin) track(sid, name, cwd string) {
	p.mu.Lock()
	p.live[sid] = liveSess{tmux: name, cwd: cwd}
	p.mu.Unlock()
}

func sliceOf(s []string) []string { return append([]string(nil), s...) }

func title(cwd string) string {
	if cwd == "" {
		return "(claude)"
	}
	return filepath.Base(cwd)
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// lastLines returns the last n non-empty lines of captured pane text, joined.
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

// lastAssistantText returns the final end_turn assistant reply from a Claude
// transcript (best effort).
func lastAssistantText(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	last := ""
	for _, ln := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		var o struct {
			Type    string `json:"type"`
			Message *struct {
				StopReason string          `json:"stop_reason"`
				Content    json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(ln), &o) != nil || o.Message == nil {
			continue
		}
		if o.Type == "assistant" && o.Message.StopReason == "end_turn" {
			if t := contentText(o.Message.Content); t != "" {
				last = t
			}
		}
	}
	if len(last) > 300 {
		last = last[:300] + "…"
	}
	return last
}

// contentText extracts text from a Claude message content (string or array of
// {type:text,text}).
func contentText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}
