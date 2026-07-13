package claudecode

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/yscr/source"
)

// fakeTmux records tmux invocations. has-session fails by default (nothing runs
// under us in tests) so the dormant/resume + discovery paths exercise; `panes`
// seeds list-panes output for pane-discovery tests.
type fakeTmux struct {
	calls   [][]string
	panes   string // list-panes -F output (empty → no discovery match)
	capture string // capture-pane -p output ("" → two default lines)
}

func (f *fakeTmux) run(_ context.Context, _ string, args ...string) (string, error) {
	f.calls = append(f.calls, args)
	switch args[0] {
	case "has-session":
		return "", fmt.Errorf("no session")
	case "list-panes":
		return f.panes, nil
	case "capture-pane":
		if f.capture != "" {
			return f.capture, nil
		}
		return "pane line one\npane line two", nil
	default:
		return "", nil
	}
}

func (f *fakeTmux) sawSeq(sub ...string) bool {
	for _, c := range f.calls {
		joined := " " + strings.Join(c, " ") + " "
		ok := true
		for _, s := range sub {
			if !strings.Contains(joined, " "+s+" ") {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// fakeHome builds a temp ~/.claude with a session index + one transcript.
func fakeHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, "sessions"), 0o755)
	write := func(p, s string) {
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(home, "sessions", "1001.json"), `{"pid":1001,"sessionId":"sess-A","cwd":"/repo/alpha","status":"busy","name":"alpha-1","updatedAt":200}`)
	write(filepath.Join(home, "sessions", "1002.json"), `{"pid":1002,"sessionId":"sess-B","cwd":"/repo/beta","status":"idle","name":"beta-1","updatedAt":100}`)
	// transcript for sess-A (project dir = cwd with / and . → -)
	write(filepath.Join(home, "projects", "-repo-alpha", "sess-A.jsonl"),
		`{"type":"user","message":{"role":"user","content":"hi"}}`+"\n"+
			`{"type":"assistant","message":{"stop_reason":"end_turn","content":[{"type":"text","text":"hello from alpha"}]}}`+"\n")
	return home
}

func newTest(home string, f *fakeTmux) *Plugin {
	p := New(Config{Home: home})
	p.exec = f.run
	p.now = func() int64 { return 7 }
	p.newID = func() string { return "new-uuid" }
	p.modTime = func(string) (int64, bool) { return 0, false } // default: no recent activity → idle
	// Deterministic tty: pid N (alive) → /dev/pts/N; pid 0 → dead.
	p.ttyOf = func(pid int) string {
		if pid == 0 {
			return ""
		}
		return fmt.Sprintf("/dev/pts/%d", pid)
	}
	p.sleep = func(time.Duration) {} // no keystroke pacing in tests
	return p
}

func TestList_FromIndex(t *testing.T) {
	p := newTest(fakeHome(t), &fakeTmux{})
	refs, err := p.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 {
		t.Fatalf("list = %+v; want 2", refs)
	}
	// Newest (sess-A, updatedAt 200) first; title = cwd basename.
	if refs[0].ID != "sess-A" || refs[0].Title != "alpha" {
		t.Errorf("refs[0] = %+v", refs[0])
	}
	if refs[1].ID != "sess-B" || refs[1].Title != "beta" {
		t.Errorf("refs[1] = %+v", refs[1])
	}
}

func TestSpawn_InDir(t *testing.T) {
	f := &fakeTmux{}
	p := newTest(fakeHome(t), f)
	ref, err := p.Spawn(context.Background(), source.SpawnSpec{Dir: "/repo/gamma", Prompt: "start work"})
	if err != nil {
		t.Fatal(err)
	}
	if ref.ID != "new-uuid" || ref.Title != "gamma" {
		t.Fatalf("spawn ref = %+v", ref)
	}
	// New session launched in the dir with a fresh --session-id, prompt sent.
	if !f.sawSeq("new-session", "-c", "/repo/gamma", "--session-id", "new-uuid") {
		t.Fatalf("spawn tmux args wrong: %v", f.calls)
	}
	if !f.sawSeq("send-keys", "start work") {
		t.Errorf("prompt not sent: %v", f.calls)
	}
}

func TestPost_ResumesDormant(t *testing.T) {
	f := &fakeTmux{}
	p := newTest(fakeHome(t), f)
	// sess-A is in the index (cwd /repo/alpha) but not running → Post resumes.
	if err := p.Post(context.Background(), "sess-A", "continue please"); err != nil {
		t.Fatal(err)
	}
	if !f.sawSeq("new-session", "-c", "/repo/alpha", "--resume", "sess-A") {
		t.Fatalf("resume not launched in cwd: %v", f.calls)
	}
	if !f.sawSeq("send-keys", "continue please") {
		t.Errorf("message not sent after resume: %v", f.calls)
	}
}

// sess-A (pid 1001 → /dev/pts/1001) is live in the user's OWN tmux pane → Post
// joins pid→tty→pane and drives it in place, never spawning a resume. sess-B
// (pid 1002) shares no cwd concern — the tty join is exact regardless of cwd.
func TestPost_DrivesAdoptedPane(t *testing.T) {
	f := &fakeTmux{panes: "" +
		"/dev/pts/1002\twork:5.0\n" +
		"/dev/pts/1001\twork:2.1\n"}
	p := newTest(fakeHome(t), f)
	if err := p.Post(context.Background(), "sess-A", "hi there"); err != nil {
		t.Fatal(err)
	}
	if f.sawSeq("new-session") {
		t.Errorf("should drive existing pane, not resume: %v", f.calls)
	}
	if !f.sawSeq("send-keys", "-t", "work:2.1", "-l", "hi there") {
		t.Errorf("did not send to adopted pane: %v", f.calls)
	}
}

// The session's process is alive but not inside tmux (no pane for its tty) →
// fall back to resume in a fresh session.
func TestPost_NoPaneResumes(t *testing.T) {
	f := &fakeTmux{panes: "/dev/pts/9999\telsewhere:0.0\n"} // not sess-A's tty
	p := newTest(fakeHome(t), f)
	if err := p.Post(context.Background(), "sess-A", "go"); err != nil {
		t.Fatal(err)
	}
	if !f.sawSeq("new-session", "--resume", "sess-A") {
		t.Errorf("unmapped session should resume in new session: %v", f.calls)
	}
}

// An adopted pane makes State report running, summarized from the pane.
func TestState_RunningFromAdoptedPane(t *testing.T) {
	f := &fakeTmux{panes: "/dev/pts/1001\twork:2.1\n"}
	p := newTest(fakeHome(t), f)
	st, err := p.State(context.Background(), "sess-A")
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != source.StatusRunning {
		t.Errorf("status = %q; want running (adopted pane)", st.Status)
	}
	if !strings.Contains(st.Summary, "pane line two") {
		t.Errorf("summary from pane = %q", st.Summary)
	}
}

// Panes joins every live session to its pane; a dead pid (ttyOf → "") drops.
func TestPanes_JoinsLiveSessionsToPanes(t *testing.T) {
	f := &fakeTmux{panes: "/dev/pts/1001\twork:2.1\n"} // sess-A in tmux; sess-B alive but not
	p := newTest(fakeHome(t), f)
	got := map[string]PaneInfo{}
	for _, pi := range p.Panes(context.Background()) {
		got[pi.SID] = pi
	}
	if len(got) != 2 {
		t.Fatalf("panes = %+v; want 2 live", got)
	}
	if a := got["sess-A"]; a.Pane != "work:2.1" || a.Status != "busy" || a.Cwd != "/repo/alpha" {
		t.Errorf("sess-A = %+v", a)
	}
	if b := got["sess-B"]; b.Pane != "" || b.Status != "idle" { // alive, no pane
		t.Errorf("sess-B = %+v", b)
	}
}

// Pane fixtures: an active AskUserQuestion selector as capture-pane renders it.
const paneSingleSelect = ` ☐ Pick
Pick a fruit?
❯ 1. Apple
     A crisp fruit.
  2. Banana
     A soft fruit.
  3. Cherry
     A small fruit.
  4. Type something.
  5. Chat about this
Enter to select · ↑/↓ to navigate · Esc to cancel`

const paneMultiSelect = `←  ☐ Toppings  ✔ Submit  →
Pick toppings
❯ 1. [ ] Cheese
     Melted cheese.
  2. [ ] Onion
  3. [ ] Mushroom
  4. [ ] Type something
     Submit
  5. Chat about this
Enter to select · ↑/↓ to navigate · Esc to cancel`

// parsePaneQuestion reads the selector into a Questionnaire; options in display
// order, the appended Type-something/Chat rows dropped.
func TestParsePaneQuestion_Single(t *testing.T) {
	q := parsePaneQuestion(paneSingleSelect)
	if q == nil {
		t.Fatal("nil; want a questionnaire")
	}
	f := q.Fields[0]
	if f.Type != source.FieldChoice || f.Prompt != "Pick a fruit?" {
		t.Errorf("field = %+v", f)
	}
	got := []string{}
	for _, o := range f.Options {
		got = append(got, o.Label)
	}
	if strings.Join(got, ",") != "Apple,Banana,Cherry" {
		t.Errorf("options = %v (dropped Type-something/Chat?)", got)
	}
}

func TestParsePaneQuestion_Multi(t *testing.T) {
	q := parsePaneQuestion(paneMultiSelect)
	if q == nil || q.Fields[0].Type != source.FieldMulti {
		t.Fatalf("want multi field; got %+v", q)
	}
	if len(q.Fields[0].Options) != 3 { // Cheese/Onion/Mushroom, not Type-something/Submit
		t.Errorf("options = %+v", q.Fields[0].Options)
	}
}

// withHook points the plugin at a fresh pending dir and drops a hook payload
// for sid into it.
func withHook(t *testing.T, p *Plugin, sid, payload string) {
	t.Helper()
	p.pendingDir = t.TempDir()
	if err := os.WriteFile(filepath.Join(p.pendingDir, sid+".json"), []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}
}

const hookPayloadJSON = `{
  "session_id":"sess-A",
  "tool_use_id":"toolu_ABC",
  "transcript_path":"%s",
  "tool_input":{"questions":[{"question":"Deploy?","header":"Deploy","multiSelect":false,
    "options":[{"label":"Staging","description":"to staging"},{"label":"Production","description":"to prod"}]}]}
}`

// A hook payload whose tool_use_id is NOT yet in the transcript → pending, with
// full structured options (no scraping).
func TestHookQuestion_Pending(t *testing.T) {
	p := newTest(fakeHome(t), &fakeTmux{})
	tp := filepath.Join(t.TempDir(), "t.jsonl")
	os.WriteFile(tp, []byte(`{"type":"user"}`+"\n"), 0o644) // no toolu_ABC yet
	withHook(t, p, "sess-A", fmt.Sprintf(hookPayloadJSON, tp))
	q := p.hookQuestion("sess-A")
	if q == nil || q.ID != "toolu_ABC" || len(q.Fields) != 1 {
		t.Fatalf("q = %+v", q)
	}
	f := q.Fields[0]
	if f.Prompt != "Deploy?" || len(f.Options) != 2 || f.Options[1].Value != "Production" || f.Options[0].Detail != "to staging" {
		t.Errorf("field = %+v", f)
	}
}

// Once the tool_use_id appears in the transcript (write-behind = answered), the
// hook question clears (returns nil, file removed).
func TestHookQuestion_AnsweredClears(t *testing.T) {
	p := newTest(fakeHome(t), &fakeTmux{})
	tp := filepath.Join(t.TempDir(), "t.jsonl")
	os.WriteFile(tp, []byte(`{"type":"user","tool_use_id":"toolu_ABC"}`+"\n"), 0o644) // answered
	withHook(t, p, "sess-A", fmt.Sprintf(hookPayloadJSON, tp))
	if q := p.hookQuestion("sess-A"); q != nil {
		t.Errorf("want nil (answered); got %+v", q)
	}
	if _, err := os.Stat(filepath.Join(p.pendingDir, "sess-A.json")); !os.IsNotExist(err) {
		t.Error("stale hook file should have been removed")
	}
}

// State promotes to awaiting_user from the hook payload (pane not required).
func TestState_AwaitingUserFromHook(t *testing.T) {
	p := newTest(fakeHome(t), &fakeTmux{}) // no pane
	tp := filepath.Join(t.TempDir(), "t.jsonl")
	os.WriteFile(tp, []byte(`{}`+"\n"), 0o644)
	withHook(t, p, "sess-A", fmt.Sprintf(hookPayloadJSON, tp))
	st, err := p.State(context.Background(), "sess-A")
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != source.StatusAwaitingUser || len(st.Pending) != 1 {
		t.Errorf("state = %+v", st)
	}
}

// Act reads the hook question (structured) and sends the chosen option's digit.
func TestAct_UsesHookQuestion(t *testing.T) {
	f := &fakeTmux{panes: "/dev/pts/1001\twork:2.1\n"} // sess-A live in a pane
	p := newTest(fakeHome(t), f)
	tp := filepath.Join(t.TempDir(), "t.jsonl")
	os.WriteFile(tp, []byte(`{}`+"\n"), 0o644)
	withHook(t, p, "sess-A", fmt.Sprintf(hookPayloadJSON, tp))
	_, err := p.Act(context.Background(), "sess-A", source.Action{
		Name: "answer_questionnaire",
		Args: map[string]any{"answers": map[string]any{"Deploy": "Production"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !f.sawSeq("send-keys", "-t", "work:2.1", "-l", "2") { // Production = option 2
		t.Errorf("did not send digit 2: %v", f.calls)
	}
}

func TestParsePaneQuestion_NoSelector(t *testing.T) {
	if q := parsePaneQuestion("just a normal shell\n❯ typing here"); q != nil {
		t.Errorf("want nil (no selector footer); got %+v", q)
	}
}

// A numbered list in the scrollback (Claude's prose) must NOT leak in as
// options — only rows inside the widget (below the ☐ header) count.
func TestParsePaneQuestion_IgnoresScrollback(t *testing.T) {
	pane := `Here are the tradeoffs:
1. Backlinks matter most for ranking
2. Freshness helps a clone look newer
3. A clone competes head-to-head
─────────────────────────
 ☐ Pick
Pick a fruit?
❯ 1. Apple
  2. Banana
  3. Type something.
Enter to select · ↑/↓ to navigate · Esc to cancel`
	q := parsePaneQuestion(pane)
	if q == nil {
		t.Fatal("nil")
	}
	got := []string{}
	for _, o := range q.Fields[0].Options {
		got = append(got, o.Label)
	}
	if strings.Join(got, ",") != "Apple,Banana" { // NOT the scrollback 1/2/3
		t.Errorf("options = %v; scrollback leaked?", got)
	}
}

// A multi-question tab prompt is surfaced read-only (no options) so no broken
// single card is offered; the preview box-drawing is stripped from labels.
func TestParsePaneQuestion_MultiQuestion(t *testing.T) {
	pane := `←  ☐ Apply model  ☐ UI scope  ✔ Submit  →
How should it apply to DNS?
❯ 1. Push live + save to config    ┌────────────────┐
                                   │ writes records │
  2. Save to config, apply on      │ survives sync  │
     Sync                          └────────────────┘
Enter to select · ↑/↓ to navigate · Tab to switch questions · Esc to cancel`
	q := parsePaneQuestion(pane)
	if q == nil {
		t.Fatal("nil; want a read-only questionnaire")
	}
	if q.Fields[0].Prompt != "How should it apply to DNS?" {
		t.Errorf("prompt = %q", q.Fields[0].Prompt)
	}
	if len(q.Fields[0].Options) != 0 {
		t.Errorf("multi-question should have no options (read-only); got %+v", q.Fields[0].Options)
	}
	if q.Fields[0].Help == "" {
		t.Error("want a Help note for the multi-question case")
	}
}

// Preview box-drawing to the right of a single-question option is stripped.
func TestParsePaneQuestion_StripsPreview(t *testing.T) {
	pane := ` ☐ Apply
How to apply?
❯ 1. Push live    ┌──────────────┐
                  │ writes now   │
  2. On sync      └──────────────┘
Enter to select · ↑/↓ to navigate · Esc to cancel`
	q := parsePaneQuestion(pane)
	if q == nil {
		t.Fatal("nil")
	}
	got := []string{}
	for _, o := range q.Fields[0].Options {
		got = append(got, o.Label)
	}
	if strings.Join(got, ",") != "Push live,On sync" {
		t.Errorf("options = %v; preview not stripped?", got)
	}
}

// State promotes a session whose live pane shows a selector to awaiting_user.
func TestState_AwaitingUserFromPane(t *testing.T) {
	f := &fakeTmux{panes: "/dev/pts/1001\twork:2.1\n", capture: paneSingleSelect}
	p := newTest(fakeHome(t), f)
	st, err := p.State(context.Background(), "sess-A")
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != source.StatusAwaitingUser {
		t.Errorf("status = %q; want awaiting_user", st.Status)
	}
	if len(st.Pending) != 1 || len(st.Pending[0].Fields[0].Options) != 3 {
		t.Errorf("pending = %+v", st.Pending)
	}
}

// Act on a single-select question sends the chosen option's on-screen digit to
// the live pane (which selects + submits) — read the pane, write via tmux.
func TestAct_SingleSelect(t *testing.T) {
	f := &fakeTmux{panes: "/dev/pts/1001\twork:2.1\n", capture: paneSingleSelect}
	p := newTest(fakeHome(t), f)
	res, err := p.Act(context.Background(), "sess-A", source.Action{
		Name: "answer_questionnaire",
		Args: map[string]any{"answers": map[string]any{"answer": "Banana"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res == "" {
		t.Error("empty result")
	}
	// "Banana" is option 2 → send-keys -t work:2.1 -l 2
	if !f.sawSeq("send-keys", "-t", "work:2.1", "-l", "2") {
		t.Errorf("did not send digit 2 to the pane: %v", f.calls)
	}
}

// Act on a multiSelect question toggles each chosen digit, then Right (→Review)
// then 1 (Submit answers).
func TestAct_MultiSelect(t *testing.T) {
	f := &fakeTmux{panes: "/dev/pts/1001\twork:2.1\n", capture: paneMultiSelect}
	p := newTest(fakeHome(t), f)
	_, err := p.Act(context.Background(), "sess-A", source.Action{
		Name: "answer_questionnaire",
		Args: map[string]any{"answers": map[string]any{"answer": []any{"Cheese", "Mushroom"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// toggle Cheese(1) and Mushroom(3), then Right, then 1
	for _, want := range [][]string{{"-l", "1"}, {"-l", "3"}, {"Right"}, {"-l", "1"}} {
		if !f.sawSeq(append([]string{"send-keys", "-t", "work:2.1"}, want...)...) {
			t.Errorf("missing keystroke %v in %v", want, f.calls)
		}
	}
}

// Act refuses when the session isn't live in a pane (nothing to type into).
func TestAct_NoPaneErrors(t *testing.T) {
	p := newTest(fakeHome(t), &fakeTmux{}) // no panes → not live
	if _, err := p.Act(context.Background(), "sess-A", source.Action{
		Name: "answer_questionnaire",
		Args: map[string]any{"answers": map[string]any{"answer": "a"}},
	}); err == nil {
		t.Fatal("expected error when session not live in a pane")
	}
}

// Act refuses when the live pane has no question on screen.
func TestAct_NoQuestionErrors(t *testing.T) {
	f := &fakeTmux{panes: "/dev/pts/1001\twork:2.1\n", capture: "just a shell prompt\n❯ "}
	p := newTest(fakeHome(t), f)
	if _, err := p.Act(context.Background(), "sess-A", source.Action{
		Name: "answer_questionnaire",
		Args: map[string]any{"answers": map[string]any{"answer": "a"}},
	}); err == nil {
		t.Fatal("expected error when no question is on screen")
	}
}

func TestState_DormantFromTranscript(t *testing.T) {
	p := newTest(fakeHome(t), &fakeTmux{})
	st, err := p.State(context.Background(), "sess-A")
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != source.StatusIdle {
		t.Errorf("status = %q; want idle (dormant)", st.Status)
	}
	if !strings.Contains(st.Summary, "hello from alpha") {
		t.Errorf("summary from transcript = %q", st.Summary)
	}
}

// A transcript written within the liveness window → running (an active CLI
// session, not driven by us).
func TestState_RunningFromRecentTranscript(t *testing.T) {
	p := newTest(fakeHome(t), &fakeTmux{})
	p.modTime = func(string) (int64, bool) { return p.now(), true } // just touched
	st, err := p.State(context.Background(), "sess-A")
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != source.StatusRunning {
		t.Errorf("status = %q; want running (recent transcript)", st.Status)
	}
}
