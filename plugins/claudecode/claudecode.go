// Package claudecode is a source plugin over the Claude Code CLI (`claude`)
// running in detached tmux windows — the "claude-code-tmux" backend. It reads
// Claude's own session metadata from the user's home dir (~/.claude) so it can:
//
//   - LIST + RESUME existing sessions: ~/.claude/sessions/*.json is Claude's
//     live/recent session index (sessionId, cwd, status, updatedAt); resume is
//     `claude --resume <sessionId>` in that session's cwd.
//   - LAUNCH new sessions in a directory: `claude --session-id <uuid>` under a
//     chosen working dir (tmux -c <dir>).
//   - DRIVE an existing pane: if a session is already live in the user's OWN
//     tmux (not one we launched), it's addressed by discovering the pane —
//     `tmux list-panes -a`, matched on the session's cwd + a running `claude`.
//     Unambiguous (exactly one match) → we send-keys/capture that pane target
//     directly instead of spawning a duplicate `--resume` session.
//
// Interaction is asynchronous — a tmux pane is a live terminal, so Spawn/Post
// return immediately; State/Observe read the pane or the JSONL transcript
// (~/.claude/projects/<enc-cwd>/<sid>.jsonl). Mechanics mirror the ccoa bridge.
package claudecode

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/iodesystems/yscr/source"
)

const sourceID = "claude-code"

var (
	_ source.Source  = (*Plugin)(nil)
	_ source.Spawner = (*Plugin)(nil)
	_ source.Actor   = (*Plugin)(nil)
)

// Config tunes the plugin. Zero value is usable (tmux + claude + yscr-cc + 25).
type Config struct {
	Tmux    string   // tmux binary; "" → "tmux"
	Command []string // the CLI + fixed args; nil → ["claude"]
	Prefix  string   // tmux session-name prefix; "" → "yscr-cc"
	Home    string   // Claude config dir; "" → $CLAUDE_CONFIG_DIR or ~/.claude
	Limit   int       // max sessions List returns (most-recent); 0 → 25
	// PendingDir is where the AskUserQuestion PreToolUse hook drops structured
	// payloads (yscr hook-question). "" → DefaultPendingDir().
	PendingDir string
}

type Plugin struct {
	tmux       string
	command    []string
	prefix     string
	home       string
	limit      int
	pendingDir string
	now        func() int64
	// exec runs a command and returns combined output. Seam for tests.
	exec  func(ctx context.Context, name string, args ...string) (string, error)
	newID func() string
	// modTime reports a file's mtime in ns (ok=false if it can't be stat'd).
	// Seam for tests so the liveness window is deterministic.
	modTime func(path string) (int64, bool)
	// ttyOf returns a live process's controlling tty (e.g. "/dev/pts/5"), or ""
	// if the pid is dead or has no pts. Seam for tests. The tty joins a Claude
	// session (indexed by pid) to the tmux pane hosting it.
	ttyOf func(pid int) string
	// sleep paces keystrokes when answering the interactive question TUI (the
	// selector needs a beat to process each key). Seam for tests (no-op).
	sleep func(time.Duration)

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
		pendingDir: cfg.PendingDir,
		now:        func() int64 { return time.Now().UnixNano() },
		exec:  realExec,
		newID: newUUID,
		modTime: func(path string) (int64, bool) {
			fi, err := os.Stat(path)
			if err != nil {
				return 0, false
			}
			return fi.ModTime().UnixNano(), true
		},
		ttyOf: defaultTTYOf,
		sleep: time.Sleep,
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
	if p.pendingDir == "" {
		p.pendingDir = DefaultPendingDir()
	}
	return p
}

// DefaultPendingDir is where the AskUserQuestion hook drops payloads and where
// the plugin reads them. Override with $YSCR_PENDING_DIR.
func DefaultPendingDir() string {
	if v := os.Getenv("YSCR_PENDING_DIR"); v != "" {
		return v
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".yscr", "pending")
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
// The file is named by the claude process's PID; Pid + the process's controlling
// tty are the join key to the tmux pane hosting the session.
type sessionMeta struct {
	Pid       int    `json:"pid"`
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
	Status    string `json:"status"` // busy | idle | shell (Claude's own label)
	Kind      string `json:"kind"`
	Name      string `json:"name"`
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
		refs = append(refs, source.SessionRef{Source: sourceID, ID: m.SessionID, Title: title(m.Cwd), Dir: m.Cwd})
	}
	return refs, nil
}

func (p *Plugin) State(ctx context.Context, sid string) (source.State, error) {
	cwd := p.cwdOf(sid)
	ref := source.SessionRef{Source: sourceID, ID: sid, Title: title(cwd), Dir: cwd}

	// Pending question, structured — the hook payload is authoritative and
	// geometry-independent. This doesn't need a live pane (though answering does).
	var pending []source.Questionnaire
	if q := p.hookQuestion(sid); q != nil {
		pending = []source.Questionnaire{*q}
	}

	// Live in a pane — ours or the user's own (exact pid→tty→pane join).
	if tgt, live := p.target(ctx, sid); live {
		pane, _ := p.tmuxCmd(ctx, "capture-pane", "-t", tgt, "-p")
		if len(pending) == 0 { // no hook → best-effort pane parse
			if q := parsePaneQuestion(pane); q != nil {
				pending = []source.Questionnaire{*q}
			}
		}
		return source.State{Ref: ref, Status: statusWith(source.StatusRunning, pending), Summary: lastLines(pane, 3), Pending: pending, UpdatedAt: p.now()}, nil
	}

	// Not live in a pane. The transcript JSONL is appended every turn, so a
	// recent mtime means "active now"; older → dormant. A hook question still
	// promotes to awaiting_user (it can be shown; answering needs the pane back).
	if cwd == "" {
		if len(pending) > 0 {
			return source.State{Ref: ref, Status: source.StatusAwaitingUser, Summary: pending[0].Intro, Pending: pending, UpdatedAt: p.now()}, nil
		}
		return source.State{}, fmt.Errorf("claude-code: unknown session %q", sid)
	}
	path := p.transcriptPath(cwd, sid)
	summary := lastAssistantText(path)
	if summary == "" {
		summary = "(no transcript)"
	}
	status := source.StatusIdle
	if mt, ok := p.modTime(path); ok && p.now()-mt < activeWindowNS {
		status = source.StatusRunning
	}
	return source.State{Ref: ref, Status: statusWith(status, pending), Summary: summary, Pending: pending, UpdatedAt: p.now()}, nil
}

// statusWith promotes a session to awaiting_user when it has a pending
// questionnaire, otherwise keeps the computed liveness status.
func statusWith(base source.Status, pending []source.Questionnaire) source.Status {
	if len(pending) > 0 {
		return source.StatusAwaitingUser
	}
	return base
}

// activeWindowNS: a transcript touched within this window means the session is
// live (an interactive CLI turn or a driven pane just wrote to it).
const activeWindowNS = int64(3 * time.Minute)

func (p *Plugin) Post(ctx context.Context, sid, message string) error {
	// Already live (our session, or the user's own pane)? Drive it in place.
	// Otherwise resume the dormant session in its cwd.
	tgt, live := p.target(ctx, sid)
	if !live {
		cwd := p.cwdOf(sid)
		if cwd == "" {
			return fmt.Errorf("claude-code: cannot resume unknown session %q", sid)
		}
		name := p.tmuxName(sid)
		if err := p.launch(ctx, name, cwd, append(sliceOf(p.command), "--resume", sid)); err != nil {
			return err
		}
		p.track(sid, name, cwd)
		tgt = name
	}
	return p.send(ctx, tgt, message)
}

// Spawn starts a NEW Claude session in spec.Dir (default: cwd).
func (p *Plugin) Spawn(ctx context.Context, spec source.SpawnSpec) (source.SessionRef, error) {
	sid := p.newID()
	name := p.tmuxName(sid)
	dir := spec.Dir
	if dir == "" {
		// Default to the user's home, NOT the daemon's cwd (which would land
		// claude in yscr's own repo tree).
		dir, _ = os.UserHomeDir()
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
	return source.SessionRef{Source: sourceID, ID: sid, Title: t, Dir: dir}, nil
}

// Observe emits the latest assistant reply once, then closes.
func (p *Plugin) Observe(ctx context.Context, sid string) (<-chan source.Event, error) {
	cwd := p.cwdOf(sid)
	ref := source.SessionRef{Source: sourceID, ID: sid, Title: title(cwd)}
	ch := make(chan source.Event, 1)
	var content string
	if tgt, live := p.target(ctx, sid); live {
		pane, _ := p.tmuxCmd(ctx, "capture-pane", "-t", tgt, "-p")
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

// ── source.Actor: answer a pending AskUserQuestion ──────────────────

// answerKeyDelay paces keystrokes into the question TUI; the selector needs a
// beat between keys (especially the multiSelect toggle → Review → submit hops).
const answerKeyDelay = 300 * time.Millisecond

// Act answers a pending question by driving the live TUI. It READS the question
// structured (hook payload; pane parse as fallback) to map each chosen option to
// its on-screen digit, then WRITES the selection as keystrokes: single-select the
// digit selects+submits; multiSelect toggles each digit, → to Review, 1 to submit.
// Only "answer_questionnaire" is supported.
func (p *Plugin) Act(ctx context.Context, sid string, action source.Action) (string, error) {
	if action.Name != "answer_questionnaire" {
		return "", fmt.Errorf("claude-code: unsupported action %q", action.Name)
	}
	qid, _ := action.Args["questionnaire_id"].(string)
	answers, _ := action.Args["answers"].(map[string]any)
	if answers == nil {
		return "", fmt.Errorf("claude-code: answers must be {field_key: value}")
	}
	// The pane must be live to receive the answer.
	tgt, live := p.target(ctx, sid)
	if !live {
		return "", fmt.Errorf("claude-code: session %q is not live in a pane; can't answer", sid)
	}
	// Read the question structured (hook), falling back to the pane.
	q := p.hookQuestion(sid)
	if q == nil {
		pane, _ := p.tmuxCmd(ctx, "capture-pane", "-t", tgt, "-p")
		q = parsePaneQuestion(pane)
	}
	if q == nil {
		return "", fmt.Errorf("claude-code: no question is pending for %q", sid)
	}
	// Guard against answering a different question than the one presented: the
	// id is a stable hash of the on-screen question + options.
	if qid != "" && qid != q.ID {
		return "", fmt.Errorf("claude-code: on-screen question changed (expected %q, now %q); re-read before answering", qid, q.ID)
	}
	keys, err := keystrokesFor(*q, answers)
	if err != nil {
		return "", err
	}
	for _, k := range keys {
		if _, err := p.tmuxCmd(ctx, append([]string{"send-keys", "-t", tgt}, k...)...); err != nil {
			return "", fmt.Errorf("claude-code: send-keys: %w", err)
		}
		p.sleep(answerKeyDelay)
	}
	return fmt.Sprintf("answered %q", q.ID), nil
}

// keystrokesFor maps a validated Answer to the sequence of tmux send-keys arg
// tails that drive the selector. Supports a SINGLE-question prompt (the common
// case); multi-question prompts use a tab UI not yet automated. Digit keys
// address options by their 1-based index, so >9 options aren't addressable.
func keystrokesFor(q source.Questionnaire, answers map[string]any) ([][]string, error) {
	if len(q.Fields) != 1 {
		return nil, fmt.Errorf("claude-code: %d-question prompts aren't auto-answerable yet — answer it in the pane", len(q.Fields))
	}
	f := q.Fields[0]
	digitOf := func(val string) (string, error) {
		for i, o := range f.Options {
			if o.Value == val {
				if i+1 > 9 {
					return "", fmt.Errorf("claude-code: option %d not keyboard-addressable (>9)", i+1)
				}
				return strconv.Itoa(i + 1), nil
			}
		}
		return "", fmt.Errorf("claude-code: %q is not an option of %q", val, f.Key)
	}
	raw, ok := answers[f.Key]
	if !ok {
		return nil, fmt.Errorf("claude-code: no answer for %q", f.Key)
	}
	var keys [][]string
	if f.Type == source.FieldMulti {
		vals := toStrings(raw)
		if len(vals) == 0 {
			return nil, fmt.Errorf("claude-code: no selections for %q", f.Key)
		}
		for _, v := range vals {
			d, err := digitOf(v)
			if err != nil {
				return nil, err
			}
			keys = append(keys, []string{"-l", d}) // toggle the checkbox
		}
		keys = append(keys, []string{"Right"})   // → the Review/Submit tab
		keys = append(keys, []string{"-l", "1"}) // "1. Submit answers"
		return keys, nil
	}
	// choice / single-select: the digit selects and submits.
	v, _ := raw.(string)
	d, err := digitOf(v)
	if err != nil {
		return nil, err
	}
	return [][]string{{"-l", d}}, nil
}

// toStrings coerces a multi-select answer value (JSON: []any of strings, or a
// lone string) to []string.
func toStrings(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		return []string{t}
	}
	return nil
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

// target resolves the tmux target to drive a session and whether it's live.
// Order: our own tracked session → the pane hosting the session in the user's
// own tmux (exact pid→tty→pane join) → none (caller resumes into a fresh
// session). Returns our own session name as the target even when not live, so a
// caller that only needs a name has one.
func (p *Plugin) target(ctx context.Context, sid string) (string, bool) {
	own := p.tmuxName(sid)
	if _, err := p.tmuxCmd(ctx, "has-session", "-t", own); err == nil {
		return own, true
	}
	if tgt, ok := p.paneOf(ctx, sid); ok {
		return tgt, true
	}
	return own, false
}

// paneOf finds the tmux pane hosting a Claude session, exactly. The session
// index (~/.claude/sessions/<pid>.json) gives the session's pid; the pid's
// controlling tty is the join key to a tmux pane (#{pane_tty}). Returns "",
// false if the process is dead or not running inside tmux. Unlike a cwd match,
// this disambiguates multiple claude sessions in the same directory.
func (p *Plugin) paneOf(ctx context.Context, sid string) (string, bool) {
	m, ok := p.readIndex()[sid]
	if !ok || m.Pid == 0 {
		return "", false
	}
	tty := p.ttyOf(m.Pid) // "" if the pid is dead
	if tty == "" {
		return "", false
	}
	tgt, ok := p.paneByTTY(ctx)[tty]
	return tgt, ok
}

// paneByTTY maps each tmux pane's controlling tty to its target address
// (session:window.pane), across every session — including the user's own.
func (p *Plugin) paneByTTY(ctx context.Context) map[string]string {
	out, err := p.tmuxCmd(ctx, "list-panes", "-a", "-F",
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

// defaultTTYOf reads a live process's controlling tty from /proc (Linux). fd/0
// of a claude CLI process is its pts; readlink fails if the pid is gone.
func defaultTTYOf(pid int) string {
	l, err := os.Readlink(fmt.Sprintf("/proc/%d/fd/0", pid))
	if err != nil || !strings.HasPrefix(l, "/dev/pts/") {
		return ""
	}
	return l
}

// PaneInfo is one adopted session in the analysis view: the Claude session, the
// tmux pane it's live in (empty if alive but not in tmux), and Claude's own
// status label.
type PaneInfo struct {
	SID    string
	Pane   string // session:window.pane, or "" if not in tmux
	Status string // Claude's label: busy | idle | shell
	Cwd    string
	Name   string
}

// Panes returns every live Claude session (process alive) joined to its tmux
// pane. This is the analysis that backs `yscr panes` and the automatic
// adoption of the user's own panes. Dead index entries are dropped.
func (p *Plugin) Panes(ctx context.Context) []PaneInfo {
	byTTY := p.paneByTTY(ctx)
	var out []PaneInfo
	for sid, m := range p.readIndex() {
		tty := p.ttyOf(m.Pid)
		if tty == "" {
			continue // process gone
		}
		out = append(out, PaneInfo{SID: sid, Pane: byTTY[tty], Status: m.Status, Cwd: m.Cwd, Name: m.Name})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Pane < out[j].Pane })
	return out
}

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

// ── pending AskUserQuestion → Questionnaire (from the PreToolUse hook) ──
//
// The robust read: the `yscr hook-question` PreToolUse hook drops the FULL
// structured tool_input to <pendingDir>/<session_id>.json the instant the
// question is presented — geometry-independent, no scraping. Preferred over the
// pane parser (which is kept as a fallback when the hook isn't installed).

type hookOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}
type hookQuestionInput struct {
	Question    string       `json:"question"`
	Header      string       `json:"header"`
	MultiSelect bool         `json:"multiSelect"`
	Options     []hookOption `json:"options"`
}
type hookPayload struct {
	ToolUseID  string `json:"tool_use_id"`
	Transcript string `json:"transcript_path"`
	ToolInput  struct {
		Questions []hookQuestionInput `json:"questions"`
	} `json:"tool_input"`
}

// hookQuestion returns the structured pending question captured by the hook for
// sid, or nil if there's none — or if it's already answered. Answered-detection
// leans on write-behind: the tool_use_id lands in the transcript ONLY after the
// turn completes, so its presence there means "answered" → we clear the file.
func (p *Plugin) hookQuestion(sid string) *source.Questionnaire {
	path := filepath.Join(p.pendingDir, sid+".json")
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var pl hookPayload
	if json.Unmarshal(b, &pl) != nil || len(pl.ToolInput.Questions) == 0 {
		return nil
	}
	if pl.ToolUseID != "" && pl.Transcript != "" {
		if tb, err := os.ReadFile(pl.Transcript); err == nil && bytes.Contains(tb, []byte(pl.ToolUseID)) {
			_ = os.Remove(path) // answered → stale
			return nil
		}
	}
	return hookToQuestionnaire(pl.ToolUseID, pl.ToolInput.Questions)
}

// hookToQuestionnaire maps the structured tool_input onto the source contract:
// one Field per question (choice/multi), options carried verbatim.
func hookToQuestionnaire(id string, qs []hookQuestionInput) *source.Questionnaire {
	q := &source.Questionnaire{ID: id, Title: "Claude is asking"}
	if len(qs) > 0 {
		q.Intro = qs[0].Question
	}
	for i, aq := range qs {
		f := source.Field{Key: aq.Header, Prompt: aq.Question, Type: source.FieldChoice, Required: true}
		if aq.MultiSelect {
			f.Type = source.FieldMulti
		}
		if f.Key == "" {
			f.Key = fmt.Sprintf("q%d", i+1)
		}
		for _, o := range aq.Options {
			f.Options = append(f.Options, source.Option{Value: o.Label, Label: o.Label, Detail: o.Description})
		}
		q.Fields = append(q.Fields, f)
	}
	return q
}

// ── pending AskUserQuestion → Questionnaire (from the live pane, fallback) ──
//
// A pending AskUserQuestion exists ONLY in the interactive TUI — Claude writes
// the tool_use to the transcript only after the turn completes (post-answer),
// so the jsonl can't be the read for a live question. We parse the captured
// pane instead. The on-screen numbering IS the keystroke to send.

// answerFieldKey is the single Field.Key a pane question projects to.
const answerFieldKey = "answer"

var (
	// a numbered option row: "❯ 1. Staging" or "  2. [ ] Cheese".
	paneOptRe = regexp.MustCompile(`^\s*[❯>]?\s*(\d+)\.\s+(.*\S)\s*$`)
	// a leading checkbox on a multiSelect option: "[ ] Cheese" / "[✔] Cheese".
	paneCheckboxRe = regexp.MustCompile(`^\[.\]\s*(.*)$`)
)

// parsePaneQuestion extracts an active AskUserQuestion selector from a captured
// pane into a Questionnaire (one Field; options in display order so option i is
// on-screen digit i+1). Returns nil when no selector is on screen.
//
// Scoping matters: option rows are read ONLY from within the widget — between
// its "☐ Title"/tab header and the footer — so numbered lists in the scrollback
// (Claude's prose "1. …") don't leak in. Multi-question prompts (a tab UI) are
// surfaced read-only (no options) since a single card can't drive their tabs.
func parsePaneQuestion(pane string) *source.Questionnaire {
	lines := strings.Split(pane, "\n")
	footer := -1
	for i, ln := range lines {
		if strings.Contains(ln, "to select") && strings.Contains(ln, "to navigate") {
			footer = i
		}
	}
	if footer < 0 {
		return nil // no active selector
	}
	// The widget header ("☐ Title", or "← ☐ Q1 ☐ Q2 ✔ Submit →" tabs) is the
	// nearest ☐/☒ line above the footer; it bounds the widget top.
	header := -1
	for i := footer - 1; i >= 0; i-- {
		if strings.ContainsAny(lines[i], "☐☒") {
			header = i
			break
		}
	}
	if header < 0 {
		return nil
	}
	// Multi-question: ≥2 checkbox tabs in the header, or the footer says so.
	multiQ := strings.Count(lines[header], "☐")+strings.Count(lines[header], "☒") >= 2 ||
		strings.Contains(lines[footer], "switch questions")

	// Options: numbered rows strictly inside the widget, minus meta rows. A
	// preview box / wrapped continuation trails the label — cut at the box rune.
	var opts []source.Option
	multiSel := false
	firstOpt := -1
	for i := header + 1; i < footer; i++ {
		m := paneOptRe.FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		label := cutPreview(m[2])
		if cb := paneCheckboxRe.FindStringSubmatch(label); cb != nil {
			multiSel = true
			label = cutPreview(cb[1])
		}
		label = strings.TrimRight(strings.TrimSpace(label), ".")
		switch strings.ToLower(label) {
		case "", "type something", "chat about this", "submit", "submit answers", "cancel":
			continue
		}
		if firstOpt < 0 {
			firstOpt = i
		}
		opts = append(opts, source.Option{Value: label, Label: label})
	}
	// Question text = last non-empty, non-rule line between the header and the
	// first option (falls back to the header→footer span).
	end := firstOpt
	if end < 0 {
		end = footer
	}
	question := "Claude is asking"
	for i := end - 1; i > header; i-- {
		t := strings.TrimSpace(lines[i])
		if t == "" || strings.Contains(t, "─") {
			continue
		}
		question = t
		break
	}
	ftype := source.FieldChoice
	if multiSel {
		ftype = source.FieldMulti
	}
	f := source.Field{Key: answerFieldKey, Prompt: question, Type: ftype, Required: true, Options: opts}
	if multiQ {
		// Not auto-answerable from one card; present read-only.
		f.Options = nil
		f.Help = "multi-question prompt — answer in the terminal or ask the concierge"
	} else if len(opts) == 0 {
		return nil // a selector with no real options isn't actionable
	}
	return &source.Questionnaire{
		ID: questionID(question, f.Options), Title: "Claude is asking", Intro: question,
		Fields: []source.Field{f},
	}
}

// cutPreview drops the AskUserQuestion preview panel / rule that renders as
// box-drawing runes to the right of (or below) an option label.
func cutPreview(s string) string {
	if i := strings.IndexAny(s, "│┌┐└┘─├┤┬┴┼╭╮╰╯"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// questionID is a stable id for a pane question so State and Act agree across
// captures of the same on-screen question (and detect if it changed).
func questionID(question string, opts []source.Option) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(question))
	for _, o := range opts {
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(o.Label))
	}
	return fmt.Sprintf("auq-%08x", h.Sum32())
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
