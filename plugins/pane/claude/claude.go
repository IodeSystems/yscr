// Package claude is the Claude Code adapter for the generic pane source: it
// supplies the program semantics (session discovery, transcript history,
// AskUserQuestion detection + answering, spawn/resume) while plugins/pane owns
// the tmux plumbing. It reads Claude's own metadata under ~/.claude:
//
//   - ~/.claude/sessions/*.json — the live/recent session index (Discover).
//   - ~/.claude/projects/<enc-cwd>/<sid>.jsonl — per-workdir transcripts
//     (State summary + History projection).
//   - <pendingDir>/<sid>.json — structured AskUserQuestion payloads dropped by
//     the `yscr hook-question` PreToolUse hook (authoritative, geometry-free).
package claude

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/iodesystems/yscr/plugins/pane"
	"github.com/iodesystems/yscr/source"
)

// SourceID is the source id claude sessions present as (SessionRef.Source).
const SourceID = "claude-code"

// program is the tmux pane_current_command for a Claude CLI pane.
const program = "claude"

var (
	_ pane.Adapter  = (*Adapter)(nil)
	_ pane.Streamer = (*Adapter)(nil)
)

// streamPollDefault is how often Stream checks the transcript for appended
// records. Claude turns are slow, so 1s is plenty; the narrator buffers anyway.
const streamPollDefault = time.Second

// Adapter implements pane.Adapter for Claude Code. It holds no tmux state — the
// source lends a pane.Tmux for pane I/O; only sessions we launch before they
// reach the index are tracked here (sid → cwd), mirroring the CLI's write delay.
type Adapter struct {
	home       string
	pendingDir string
	command    []string
	now        func() int64
	modTime    func(path string) (int64, bool)
	newID      func() string
	sleep      func(time.Duration)
	streamPoll time.Duration // transcript tail cadence; overridable in tests

	mu      sync.Mutex
	tracked map[string]string // sid → cwd for sessions launched pre-index
}

// Config tunes the adapter. Zero value is usable (claude + ~/.claude + default
// pending dir).
type Config struct {
	Command    []string // the CLI + fixed args; nil → ["claude"]
	Home       string   // Claude config dir; "" → $CLAUDE_CONFIG_DIR or ~/.claude
	PendingDir string   // AskUserQuestion hook drop dir; "" → DefaultPendingDir()
}

func New(cfg Config) *Adapter {
	a := &Adapter{
		home:       cfg.Home,
		pendingDir: cfg.PendingDir,
		command:    cfg.Command,
		now:        func() int64 { return time.Now().UnixNano() },
		modTime: func(path string) (int64, bool) {
			fi, err := os.Stat(path)
			if err != nil {
				return 0, false
			}
			return fi.ModTime().UnixNano(), true
		},
		newID:      newUUID,
		sleep:      time.Sleep,
		streamPoll: streamPollDefault,
		tracked:    map[string]string{},
	}
	if len(a.command) == 0 {
		a.command = []string{"claude"}
	}
	if a.home == "" {
		if v := os.Getenv("CLAUDE_CONFIG_DIR"); v != "" {
			a.home = v
		} else {
			h, _ := os.UserHomeDir()
			a.home = filepath.Join(h, ".claude")
		}
	}
	if a.pendingDir == "" {
		a.pendingDir = DefaultPendingDir()
	}
	return a
}

// DefaultPendingDir is where the AskUserQuestion hook drops payloads and where
// the adapter reads them. Override with $YSCR_PENDING_DIR.
func DefaultPendingDir() string {
	if v := os.Getenv("YSCR_PENDING_DIR"); v != "" {
		return v
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".yscr", "pending")
}

func (a *Adapter) ID() string               { return SourceID }
func (a *Adapter) Handles(prog string) bool { return prog == program }

// ── `yscr panes` diagnostic ─────────────────────────────────────────

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

// Panes joins every live Claude session (process alive) to its tmux pane — the
// analysis backing `yscr panes`. Diagnostic only: it shells to tmux directly
// (no source plumbing). Dead index entries are dropped.
func (a *Adapter) Panes(ctx context.Context) []PaneInfo {
	byTTY := map[string]string{}
	out, _ := exec.CommandContext(ctx, "tmux", "list-panes", "-a", "-F",
		"#{pane_tty}\t#{session_name}:#{window_index}.#{pane_index}").CombinedOutput()
	for _, ln := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if f := strings.SplitN(ln, "\t", 2); len(f) == 2 && f[0] != "" {
			byTTY[f[0]] = f[1]
		}
	}
	var infos []PaneInfo
	for sid, m := range a.readIndex() {
		tty := defaultTTYOf(m.Pid)
		if tty == "" {
			continue // process gone
		}
		infos = append(infos, PaneInfo{SID: sid, Pane: byTTY[tty], Status: m.Status, Cwd: m.Cwd, Name: m.Name})
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Pane < infos[j].Pane })
	return infos
}

// ── discovery (~/.claude/sessions) ──────────────────────────────────

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
func (a *Adapter) readIndex() map[string]sessionMeta {
	out := map[string]sessionMeta{}
	files, _ := filepath.Glob(filepath.Join(a.home, "sessions", "*.json"))
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

// Discover enumerates claude sessions from the index, plus any we launched that
// haven't reached the index yet (tracked). Each carries Pid for the pid↔pane join.
func (a *Adapter) Discover(_ context.Context) []pane.Session {
	metas := a.readIndex()
	a.mu.Lock()
	for sid, cwd := range a.tracked {
		if _, ok := metas[sid]; !ok {
			metas[sid] = sessionMeta{SessionID: sid, Cwd: cwd, UpdatedAt: a.now()}
		}
	}
	a.mu.Unlock()
	out := make([]pane.Session, 0, len(metas))
	for _, m := range metas {
		out = append(out, pane.Session{
			Source: SourceID, ID: m.SessionID, Cwd: m.Cwd, Name: title(m.Cwd),
			Pid: m.Pid, UpdatedAt: m.UpdatedAt,
		})
	}
	return out
}

// projectDir is where Claude stores a workdir's transcripts.
func (a *Adapter) projectDir(cwd string) string {
	repl := strings.NewReplacer("/", "-", ".", "-")
	return filepath.Join(a.home, "projects", repl.Replace(cwd))
}

func (a *Adapter) transcriptPath(cwd, sid string) string {
	return filepath.Join(a.projectDir(cwd), sid+".jsonl")
}

// cwdOf resolves a session's working dir: the session's own Cwd, else our
// tracked map, else the index.
func (a *Adapter) cwdOf(s pane.Session) string {
	if s.Cwd != "" {
		return s.Cwd
	}
	a.mu.Lock()
	if cwd, ok := a.tracked[s.ID]; ok && cwd != "" {
		a.mu.Unlock()
		return cwd
	}
	a.mu.Unlock()
	return a.readIndex()[s.ID].Cwd
}

func (a *Adapter) track(sid, cwd string) {
	a.mu.Lock()
	a.tracked[sid] = cwd
	a.mu.Unlock()
}

// ── State ───────────────────────────────────────────────────────────

func (a *Adapter) State(ctx context.Context, s pane.Session, t pane.Tmux) (source.State, error) {
	sid := s.ID
	cwd := a.cwdOf(s)
	ref := source.SessionRef{Source: SourceID, ID: sid, Title: title(cwd), Dir: cwd}

	// Pending question, structured — the hook payload is authoritative and
	// geometry-independent. Doesn't need a live pane (though answering does).
	var pending []source.Questionnaire
	if q := a.hookQuestion(sid); q != nil {
		pending = []source.Questionnaire{*q}
	}

	// Live in a pane — ours or the user's own (exact pid→tty→pane join).
	if tgt, live := t.Target(ctx, s); live {
		capr, _ := t.Capture(ctx, tgt)
		if len(pending) == 0 { // no hook → best-effort pane parse
			if q := parsePaneQuestion(capr); q != nil {
				pending = []source.Questionnaire{*q}
			}
		}
		return source.State{Ref: ref, Status: statusWith(source.StatusRunning, pending), Summary: lastLines(capr, 3), Pending: pending, UpdatedAt: a.now()}, nil
	}

	// Not live. The transcript JSONL is appended every turn, so a recent mtime
	// means "active now"; older → dormant. A hook question still promotes to
	// awaiting_user (it can be shown; answering needs the pane back).
	if cwd == "" {
		if len(pending) > 0 {
			return source.State{Ref: ref, Status: source.StatusAwaitingUser, Summary: pending[0].Intro, Pending: pending, UpdatedAt: a.now()}, nil
		}
		return source.State{}, fmt.Errorf("claude-code: unknown session %q", sid)
	}
	path := a.transcriptPath(cwd, sid)
	summary := lastAssistantText(path)
	if summary == "" {
		summary = "(no transcript)"
	}
	status := source.StatusIdle
	if mt, ok := a.modTime(path); ok && a.now()-mt < activeWindowNS {
		status = source.StatusRunning
	}
	return source.State{Ref: ref, Status: statusWith(status, pending), Summary: summary, Pending: pending, UpdatedAt: a.now()}, nil
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

// ── History ─────────────────────────────────────────────────────────

// History projects the JSONL transcript to compact width-invariant turn lines.
// The Tmux handle is unused — claude's history is the file, not the pane.
func (a *Adapter) History(_ context.Context, s pane.Session, n int, _ pane.Tmux) (string, error) {
	if n <= 0 {
		n = 12
	}
	cwd := a.cwdOf(s)
	if cwd == "" {
		return "", fmt.Errorf("claude-code: unknown session %q", s.ID)
	}
	path := a.transcriptPath(cwd, s.ID)
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("claude-code: no transcript for %q: %w", s.ID, err)
	}
	turns := projectTranscript(b)
	if len(turns) == 0 {
		return "(no conversation yet)", nil
	}
	if len(turns) > n {
		turns = turns[len(turns)-n:]
	}
	return strings.Join(turns, "\n"), nil
}

// Stream implements pane.Streamer: it tails the JSONL transcript from its
// current end, emitting one event per newly-appended turn (projected the same
// way History projects — assistant/user text + tool-call summaries, thinking and
// tool_result bodies dropped). This lets a claude session feed the narration
// path just like a terminal pane. The Tmux handle is unused — the transcript,
// not the pane, is the source. Runs until ctx is cancelled.
func (a *Adapter) Stream(ctx context.Context, s pane.Session, _ pane.Tmux) (<-chan source.Event, error) {
	ref := source.SessionRef{Source: SourceID, ID: s.ID, Title: title(a.cwdOf(s)), Dir: a.cwdOf(s)}
	cwd := a.cwdOf(s)
	if cwd == "" {
		ch := make(chan source.Event)
		close(ch)
		return ch, nil
	}
	path := a.transcriptPath(cwd, s.ID)
	out := make(chan source.Event)
	// Open + seek to the current end SYNCHRONOUSLY so the start point is fixed at
	// call time — any turn appended after Stream returns is captured (no race with
	// the goroutine's seek).
	f, err := os.Open(path)
	if err != nil {
		close(out)
		return out, nil // no transcript yet — nothing to tail
	}
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		f.Close()
		close(out)
		return out, nil
	}
	go func() {
		defer close(out)
		defer f.Close()
		ticker := time.NewTicker(a.streamPoll)
		defer ticker.Stop()
		buf := make([]byte, 0, 8*1024)
		chunk := make([]byte, 32*1024)
		emit := func(line string) bool {
			t, ok := projectRecord(line)
			if !ok {
				return true
			}
			select {
			case out <- source.Event{Ref: ref, Kind: source.EventProgress, Content: t, At: a.now()}:
				return true
			case <-ctx.Done():
				return false
			}
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// Drain everything appended since the last read (os.File keeps its
				// offset; a Read after the file grew returns the new bytes).
				for {
					n, _ := f.Read(chunk)
					if n == 0 {
						break
					}
					buf = append(buf, chunk[:n]...)
				}
				// Process only COMPLETE lines; keep a partial trailing line buffered.
				for {
					i := bytes.IndexByte(buf, '\n')
					if i < 0 {
						break
					}
					line := string(buf[:i])
					buf = buf[i+1:]
					if !emit(line) {
						return
					}
				}
			}
		}
	}()
	return out, nil
}

// projectTranscript renders a Claude JSONL transcript to compact turn lines:
// user/assistant text plus a one-token summary of each tool call. It DROPS the
// bulk — chain-of-thought (thinking), tool_result bodies, and non-message
// records — keeping magnitude (which tools ran) without the bytes. Oldest→newest.
func projectTranscript(b []byte) []string {
	var turns []string
	for _, ln := range strings.Split(string(b), "\n") {
		if t, ok := projectRecord(ln); ok {
			turns = append(turns, t)
		}
	}
	return turns
}

// projectRecord projects ONE JSONL record to a compact turn line, or ("",false)
// for records that carry no turn (thinking, tool_result echoes, non-message).
// Shared by whole-transcript History and the incremental Stream tail.
func projectRecord(ln string) (string, bool) {
	if strings.TrimSpace(ln) == "" {
		return "", false
	}
	var o struct {
		Type    string `json:"type"`
		Message *struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal([]byte(ln), &o) != nil || o.Message == nil {
		return "", false
	}
	switch o.Type {
	case "user":
		if t := clip(contentText(o.Message.Content), 500); t != "" {
			return "user: " + t, true
		}
	case "assistant":
		var parts []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		}
		if json.Unmarshal(o.Message.Content, &parts) != nil {
			return "", false
		}
		var line strings.Builder
		for _, pt := range parts {
			switch pt.Type {
			case "text":
				if t := strings.TrimSpace(pt.Text); t != "" {
					line.WriteString(t)
				}
			case "tool_use":
				line.WriteString("  ⟶ " + pt.Name + toolHint(pt.Input))
			}
			// thinking / redacted_thinking: dropped.
		}
		if s := clip(strings.TrimSpace(line.String()), 500); s != "" {
			return "claude: " + s, true
		}
	}
	return "", false
}

// toolHint pulls one short scalar from a tool_use input for a "(hint)" suffix.
func toolHint(input json.RawMessage) string {
	var m map[string]any
	if json.Unmarshal(input, &m) != nil {
		return ""
	}
	for _, k := range []string{"command", "file_path", "path", "pattern", "url", "description", "prompt"} {
		if s, ok := m[k].(string); ok && s != "" {
			return "(" + clip(strings.TrimSpace(s), 60) + ")"
		}
	}
	return ""
}

// clip collapses whitespace and trims s to n runes with an ellipsis if cut.
func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// lastAssistantText returns the final end_turn assistant reply from a transcript.
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

// contentText extracts text from a message content (string or array of {text}).
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

// lastLines returns the last n non-empty lines of captured pane text, joined.
func lastLines(paneText string, n int) string {
	lines := strings.Split(strings.TrimRight(paneText, "\n"), "\n")
	var kept []string
	for i := len(lines) - 1; i >= 0 && len(kept) < n; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			kept = append([]string{lines[i]}, kept...)
		}
	}
	return strings.Join(kept, "\n")
}

// ── Post / Spawn ────────────────────────────────────────────────────

func (a *Adapter) Post(ctx context.Context, s pane.Session, message string, t pane.Tmux) error {
	// Already live (our window, or the user's own pane)? Drive it in place.
	// Otherwise resume the dormant session in its cwd.
	tgt, live := t.Target(ctx, s)
	if !live {
		cwd := a.cwdOf(s)
		if cwd == "" {
			return fmt.Errorf("claude-code: cannot resume unknown session %q", s.ID)
		}
		var err error
		tgt, err = t.Launch(ctx, s, cwd, append(sliceOf(a.command), "--resume", s.ID))
		if err != nil {
			return err
		}
		a.track(s.ID, cwd)
	}
	return send(ctx, t, tgt, message)
}

// Spawn starts a NEW Claude session in spec.Dir (default: the user's home).
func (a *Adapter) Spawn(ctx context.Context, spec source.SpawnSpec, t pane.Tmux) (pane.Session, error) {
	sid := a.newID()
	dir := spec.Dir
	if dir == "" {
		dir, _ = os.UserHomeDir()
	}
	s := pane.Session{Source: SourceID, ID: sid, Cwd: dir, Name: firstNonEmpty(spec.Title, title(dir))}
	tgt, err := t.Launch(ctx, s, dir, append(sliceOf(a.command), "--session-id", sid))
	if err != nil {
		return pane.Session{}, err
	}
	a.track(sid, dir)
	if spec.Prompt != "" {
		_ = send(ctx, t, tgt, spec.Prompt)
	}
	return s, nil
}

// send delivers a message + submit to a live pane. NOTE: send-keys -l passes an
// embedded newline through as Enter (a multi-line message submits early) — the
// paste-buffer fix is a separate slice.
func send(ctx context.Context, t pane.Tmux, target, message string) error {
	if err := t.SendKeys(ctx, target, "-l", message); err != nil {
		return err
	}
	return t.SendKeys(ctx, target, "Enter")
}

// ── Act: answer a pending AskUserQuestion ───────────────────────────

// answerKeyDelay paces keystrokes into the question TUI; the selector needs a
// beat between keys (especially the multiSelect toggle → Review → submit hops).
const answerKeyDelay = 300 * time.Millisecond

// Act answers a pending question by driving the live TUI. It READS the question
// structured (hook payload; pane parse as fallback) to map each chosen option to
// its on-screen digit, then WRITES the selection as keystrokes.
func (a *Adapter) Act(ctx context.Context, s pane.Session, action source.Action, t pane.Tmux) (string, error) {
	if action.Name != "answer_questionnaire" {
		return "", fmt.Errorf("claude-code: unsupported action %q", action.Name)
	}
	qid, _ := action.Args["questionnaire_id"].(string)
	answers, _ := action.Args["answers"].(map[string]any)
	if answers == nil {
		return "", fmt.Errorf("claude-code: answers must be {field_key: value}")
	}
	tgt, live := t.Target(ctx, s)
	if !live {
		return "", fmt.Errorf("claude-code: session %q is not live in a pane; can't answer", s.ID)
	}
	q := a.hookQuestion(s.ID)
	if q == nil {
		capr, _ := t.Capture(ctx, tgt)
		q = parsePaneQuestion(capr)
	}
	if q == nil {
		return "", fmt.Errorf("claude-code: no question is pending for %q", s.ID)
	}
	if qid != "" && qid != q.ID {
		return "", fmt.Errorf("claude-code: on-screen question changed (expected %q, now %q); re-read before answering", qid, q.ID)
	}
	keys, err := keystrokesFor(*q, answers)
	if err != nil {
		return "", err
	}
	for _, k := range keys {
		if err := t.SendKeys(ctx, tgt, k...); err != nil {
			return "", fmt.Errorf("claude-code: send-keys: %w", err)
		}
		a.sleep(answerKeyDelay)
	}
	// Verify the submit registered. Prompts that end on a Review tab (multi-
	// question, or any multi-select) show "Submit answers"/"Ready to submit"; if
	// that's still on screen after we sent the submit key, a keystroke was
	// intercepted — an "n to add notes" affordance or a changed selector — and the
	// answer did NOT go through. Report it instead of falsely claiming success.
	if endsOnReview(*q) {
		a.sleep(submitSettleDelay)
		if after, _ := t.Capture(ctx, tgt); reviewStillOpen(after) {
			return "", fmt.Errorf("claude-code: submit didn't complete for %q — the review screen is still up (a notes prompt or changed selector may have intercepted a key); answer it in the pane", q.ID)
		}
	}
	return fmt.Sprintf("answered %q", q.ID), nil
}

// submitSettleDelay lets Claude accept the answer + redraw before we verify it.
const submitSettleDelay = 600 * time.Millisecond

// endsOnReview reports whether answering q lands on the Review/Submit tab (so a
// post-submit verification is meaningful) — a multi-question prompt or any
// multi-select. A lone single-select submits directly with no review.
func endsOnReview(q source.Questionnaire) bool {
	if len(q.Fields) > 1 {
		return true
	}
	return len(q.Fields) == 1 && q.Fields[0].Type == source.FieldMulti
}

// reviewStillOpen detects the AskUserQuestion Review tab lingering after a submit
// — its markers are unambiguous (they appear nowhere else), so this won't false-
// positive on a fresh follow-up question.
func reviewStillOpen(pane string) bool {
	return strings.Contains(pane, "Submit answers") || strings.Contains(pane, "Ready to submit")
}

// keystrokesFor maps a validated Answer to the sequence of send-keys arg tails
// that drive Claude's AskUserQuestion selector. Digit keys address options by
// 1-based index within each question (so >9 options aren't addressable).
//
// The selector protocol (verified against the live TUI):
//   - single-select question: the digit SELECTS and auto-advances (to the next
//     question, or — for a lone single-select question — submits immediately);
//   - multi-select question: each digit TOGGLES a checkbox, then an advance key
//     moves on (Tab between questions in a multi-question prompt; Right to the
//     Review tab in a lone multi-select question);
//   - a multi-question or multi-select prompt lands on a Review tab, submitted
//     with "1" (Submit answers). A lone single-select question needs no submit.
func keystrokesFor(q source.Questionnaire, answers map[string]any) ([][]string, error) {
	if len(q.Fields) == 0 {
		return nil, fmt.Errorf("claude-code: no questions to answer")
	}
	multiQuestion := len(q.Fields) > 1
	digitOf := func(f source.Field, val string) (string, error) {
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

	var keys [][]string
	// A lone single-select question auto-submits on its digit; every other shape
	// (multi-question, or any multi-select) ends on a Review tab needing "1".
	autoSubmits := !multiQuestion && q.Fields[0].Type != source.FieldMulti

	for _, f := range q.Fields {
		raw, ok := answers[f.Key]
		if !ok {
			return nil, fmt.Errorf("claude-code: no answer for %q", f.Key)
		}
		if f.Type == source.FieldMulti {
			vals := toStrings(raw)
			if len(vals) == 0 {
				return nil, fmt.Errorf("claude-code: no selections for %q", f.Key)
			}
			for _, v := range vals {
				d, err := digitOf(f, v)
				if err != nil {
					return nil, err
				}
				keys = append(keys, []string{"-l", d}) // toggle
			}
			// Advance off this question: Tab to the next question tab (multi-
			// question), or Right to the Review tab (lone multi-select question).
			if multiQuestion {
				keys = append(keys, []string{"Tab"})
			} else {
				keys = append(keys, []string{"Right"})
			}
		} else {
			v, _ := raw.(string)
			d, err := digitOf(f, v)
			if err != nil {
				return nil, err
			}
			keys = append(keys, []string{"-l", d}) // selects + auto-advances
		}
	}
	if !autoSubmits {
		keys = append(keys, []string{"-l", "1"}) // Submit answers
	}
	return keys, nil
}

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

// ── pending AskUserQuestion → Questionnaire (from the PreToolUse hook) ──

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
// sid, or nil if none — or if already answered (the tool_use_id lands in the
// transcript only after the turn completes, so its presence there means done).
func (a *Adapter) hookQuestion(sid string) *source.Questionnaire {
	path := filepath.Join(a.pendingDir, sid+".json")
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
			_ = os.Remove(path)
			return nil
		}
	}
	return hookToQuestionnaire(pl.ToolUseID, pl.ToolInput.Questions)
}

// hookToQuestionnaire maps the structured tool_input onto the source contract.
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

const answerFieldKey = "answer"

var (
	paneOptRe      = regexp.MustCompile(`^\s*[❯>]?\s*(\d+)\.\s+(.*\S)\s*$`)
	paneCheckboxRe = regexp.MustCompile(`^\[.\]\s*(.*)$`)
)

// parsePaneQuestion extracts an active AskUserQuestion selector from a captured
// pane into a Questionnaire (one Field; options in display order so option i is
// on-screen digit i+1). Returns nil when no selector is on screen.
func parsePaneQuestion(paneText string) *source.Questionnaire {
	lines := strings.Split(paneText, "\n")
	footer := -1
	for i, ln := range lines {
		if strings.Contains(ln, "to select") && strings.Contains(ln, "to navigate") {
			footer = i
		}
	}
	if footer < 0 {
		return nil
	}
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
	multiQ := strings.Count(lines[header], "☐")+strings.Count(lines[header], "☒") >= 2 ||
		strings.Contains(lines[footer], "switch questions")

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
		f.Options = nil
		f.Help = "multi-question prompt — answer in the terminal or ask the concierge"
	} else if len(opts) == 0 {
		return nil
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

// ── helpers ─────────────────────────────────────────────────────────

func sliceOf(s []string) []string { return append([]string(nil), s...) }

func title(cwd string) string {
	if cwd == "" {
		return "(claude)"
	}
	return filepath.Base(cwd)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// defaultTTYOf reads a live process's controlling tty from /proc (Linux); ""
// if the pid is gone or has no pts. Used by the Panes diagnostic.
func defaultTTYOf(pid int) string {
	l, err := os.Readlink(fmt.Sprintf("/proc/%d/fd/0", pid))
	if err != nil || !strings.HasPrefix(l, "/dev/pts/") {
		return ""
	}
	return l
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
