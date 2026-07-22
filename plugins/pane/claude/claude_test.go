package claude

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/yscr/plugins/pane"
	"github.com/iodesystems/yscr/source"
)

// fakeTmux implements pane.Tmux. `live` maps a session id to its target when the
// session is live in a pane; Launch/SendKeys are recorded for assertions.
type fakeTmux struct {
	calls   [][]string
	live    map[string]string // sid → target (live)
	capture string
}

func (f *fakeTmux) Target(_ context.Context, s pane.Session) (string, bool) {
	if t, ok := f.live[s.ID]; ok {
		return t, true
	}
	return "yscr-cc-" + s.ID, false
}

func (f *fakeTmux) Capture(_ context.Context, _ string) (string, error) {
	if f.capture != "" {
		return f.capture, nil
	}
	return "pane line one\npane line two", nil
}

func (f *fakeTmux) Scrollback(_ context.Context, _ string, _ int) (string, error) {
	return f.capture, nil
}

func (f *fakeTmux) SendKeys(_ context.Context, target string, keys ...string) error {
	f.calls = append(f.calls, append([]string{"send-keys", "-t", target}, keys...))
	return nil
}

func (f *fakeTmux) Launch(_ context.Context, s pane.Session, dir string, argv []string) (string, error) {
	name := "yscr-cc-" + s.ID
	f.calls = append(f.calls, append([]string{"new-session", "-c", dir, "-s", name}, argv...))
	return name, nil
}

func (f *fakeTmux) Pipe(context.Context, string) (<-chan []byte, func(), error) {
	ch := make(chan []byte)
	close(ch)
	return ch, func() {}, nil
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
	// transcript for sess-A: the record kinds History must project (user/assistant
	// text, tool_use) and the ones it must DROP (thinking, tool_result echoes).
	write(filepath.Join(home, "projects", "-repo-alpha", "sess-A.jsonl"),
		`{"type":"user","message":{"role":"user","content":"hi"}}`+"\n"+
			`{"type":"assistant","message":{"stop_reason":"end_turn","content":[{"type":"text","text":"hello from alpha"}]}}`+"\n"+
			`{"type":"user","message":{"role":"user","content":"run the build"}}`+"\n"+
			`{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"secret plan"},{"type":"text","text":"on it"},{"type":"tool_use","name":"Bash","input":{"command":"go build ./..."}}]}}`+"\n"+
			`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"BUILD OK"}]}}`+"\n"+
			`{"type":"assistant","message":{"stop_reason":"end_turn","content":[{"type":"text","text":"build passed"}]}}`+"\n")
	return home
}

func newAdapter(home string) *Adapter {
	a := New(Config{Home: home})
	a.now = func() int64 { return 7 }
	a.newID = func() string { return "new-uuid" }
	a.modTime = func(string) (int64, bool) { return 0, false } // default: idle
	a.sleep = func(time.Duration) {}
	a.streamPoll = 5 * time.Millisecond // fast tail for tests
	return a
}

// sessA is the discovered session for sess-A (cwd + pid from the index).
func sessA() pane.Session {
	return pane.Session{Source: SourceID, ID: "sess-A", Cwd: "/repo/alpha", Pid: 1001}
}

func TestDiscover_FromIndex(t *testing.T) {
	a := newAdapter(fakeHome(t))
	got := map[string]pane.Session{}
	for _, s := range a.Discover(context.Background()) {
		got[s.ID] = s
	}
	if len(got) != 2 {
		t.Fatalf("discover = %+v; want 2", got)
	}
	if got["sess-A"].Cwd != "/repo/alpha" || got["sess-A"].Pid != 1001 {
		t.Errorf("sess-A = %+v", got["sess-A"])
	}
}

func TestSpawn_InDir(t *testing.T) {
	a := newAdapter(fakeHome(t))
	f := &fakeTmux{}
	s, err := a.Spawn(context.Background(), source.SpawnSpec{Dir: "/repo/gamma", Prompt: "start work"}, f)
	if err != nil {
		t.Fatal(err)
	}
	if s.ID != "new-uuid" || s.Name != "gamma" {
		t.Fatalf("spawn session = %+v", s)
	}
	if !f.sawSeq("new-session", "-c", "/repo/gamma", "--session-id", "new-uuid") {
		t.Fatalf("spawn tmux args wrong: %v", f.calls)
	}
	if !f.sawSeq("send-keys", "start work") {
		t.Errorf("prompt not sent: %v", f.calls)
	}
}

func TestPost_ResumesDormant(t *testing.T) {
	a := newAdapter(fakeHome(t))
	f := &fakeTmux{} // sess-A not live → resume in cwd
	if err := a.Post(context.Background(), sessA(), "continue please", f); err != nil {
		t.Fatal(err)
	}
	if !f.sawSeq("new-session", "-c", "/repo/alpha", "--resume", "sess-A") {
		t.Fatalf("resume not launched in cwd: %v", f.calls)
	}
	if !f.sawSeq("send-keys", "continue please") {
		t.Errorf("message not sent after resume: %v", f.calls)
	}
}

func TestPost_DrivesLivePane(t *testing.T) {
	a := newAdapter(fakeHome(t))
	f := &fakeTmux{live: map[string]string{"sess-A": "work:2.1"}}
	if err := a.Post(context.Background(), sessA(), "hi there", f); err != nil {
		t.Fatal(err)
	}
	if f.sawSeq("new-session") {
		t.Errorf("should drive live pane, not resume: %v", f.calls)
	}
	if !f.sawSeq("send-keys", "-t", "work:2.1", "-l", "hi there") {
		t.Errorf("did not send to live pane: %v", f.calls)
	}
}

func TestState_RunningFromLivePane(t *testing.T) {
	a := newAdapter(fakeHome(t))
	f := &fakeTmux{live: map[string]string{"sess-A": "work:2.1"}}
	st, err := a.State(context.Background(), sessA(), f)
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != source.StatusRunning {
		t.Errorf("status = %q; want running (live pane)", st.Status)
	}
	if !strings.Contains(st.Summary, "pane line two") {
		t.Errorf("summary from pane = %q", st.Summary)
	}
}

func TestState_DormantFromTranscript(t *testing.T) {
	a := newAdapter(fakeHome(t))
	st, err := a.State(context.Background(), sessA(), &fakeTmux{})
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != source.StatusIdle {
		t.Errorf("status = %q; want idle (dormant)", st.Status)
	}
	if !strings.Contains(st.Summary, "build passed") {
		t.Errorf("summary from transcript = %q", st.Summary)
	}
}

func TestState_RunningFromRecentTranscript(t *testing.T) {
	a := newAdapter(fakeHome(t))
	a.modTime = func(string) (int64, bool) { return a.now(), true } // just touched
	st, err := a.State(context.Background(), sessA(), &fakeTmux{})
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != source.StatusRunning {
		t.Errorf("status = %q; want running (recent transcript)", st.Status)
	}
}

// ── History ─────────────────────────────────────────────────────────

func TestHistory_ProjectsAndDropsBulk(t *testing.T) {
	a := newAdapter(fakeHome(t))
	got, err := a.History(context.Background(), sessA(), 0, &fakeTmux{})
	if err != nil {
		t.Fatal(err)
	}
	want := "user: hi\n" +
		"claude: hello from alpha\n" +
		"user: run the build\n" +
		"claude: on it ⟶ Bash(go build ./...)\n" +
		"claude: build passed"
	if got != want {
		t.Fatalf("History =\n%q\nwant\n%q", got, want)
	}
	if strings.Contains(got, "secret plan") {
		t.Error("thinking leaked into history")
	}
	if strings.Contains(got, "BUILD OK") {
		t.Error("tool_result body leaked into history")
	}
}

func TestHistory_LimitKeepsMostRecent(t *testing.T) {
	a := newAdapter(fakeHome(t))
	got, err := a.History(context.Background(), sessA(), 2, &fakeTmux{})
	if err != nil {
		t.Fatal(err)
	}
	want := "claude: on it ⟶ Bash(go build ./...)\nclaude: build passed"
	if got != want {
		t.Fatalf("History(n=2) =\n%q\nwant\n%q", got, want)
	}
}

func TestHistory_UnknownSession(t *testing.T) {
	a := newAdapter(fakeHome(t))
	if _, err := a.History(context.Background(), pane.Session{ID: "nope"}, 0, &fakeTmux{}); err == nil {
		t.Fatal("want error for unknown session")
	}
}

// ── Stream (JSONL tail → narration events) ──────────────────────────

// Stream starts from the CURRENT end of the transcript and emits one projected
// event per newly-appended turn — dropping thinking + tool_result records.
func TestStream_TailsAppendedTurns(t *testing.T) {
	home := fakeHome(t)
	a := newAdapter(home)
	path := filepath.Join(home, "projects", "-repo-alpha", "sess-A.jsonl")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := a.Stream(ctx, sessA(), &fakeTmux{})
	if err != nil {
		t.Fatal(err)
	}

	// Append AFTER Stream started — only these should be emitted (not the fixture
	// history that was already in the file at seek time).
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	appendRec := func(s string) {
		if _, err := f.WriteString(s + "\n"); err != nil {
			t.Fatal(err)
		}
	}
	appendRec(`{"type":"user","message":{"role":"user","content":"ship it"}}`)
	appendRec(`{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"hmm"},{"type":"text","text":"on it"},{"type":"tool_use","name":"Bash","input":{"command":"go test ./..."}}]}}`)
	appendRec(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"PASS"}]}}`) // dropped (tool_result echo)
	f.Close()

	var got []string
	deadline := time.After(2 * time.Second)
	for len(got) < 2 {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatalf("stream closed early; got %v", got)
			}
			got = append(got, ev.Content)
		case <-deadline:
			t.Fatalf("timed out; got %v", got)
		}
	}
	want := []string{"user: ship it", "claude: on it ⟶ Bash(go test ./...)"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("stream events = %v; want %v", got, want)
	}
	for _, g := range got {
		if strings.Contains(g, "hmm") || strings.Contains(g, "PASS") {
			t.Errorf("dropped record leaked: %q", g)
		}
	}
}

// A session with no transcript yields an immediately-closed channel.
func TestStream_NoTranscriptCloses(t *testing.T) {
	a := newAdapter(fakeHome(t))
	ch, err := a.Stream(context.Background(), pane.Session{Source: SourceID, ID: "sess-A", Cwd: "/repo/none"}, &fakeTmux{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := <-ch; ok {
		t.Error("stream with no transcript should be an empty closed channel")
	}
}

// Cancelling ctx ends the tail.
func TestStream_CtxCancelEnds(t *testing.T) {
	a := newAdapter(fakeHome(t))
	ctx, cancel := context.WithCancel(context.Background())
	ch, _ := a.Stream(ctx, sessA(), &fakeTmux{})
	cancel()
	for range ch { // drains to close
	}
}

// ── AskUserQuestion: hook + pane parse + Act ────────────────────────

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
	if len(q.Fields[0].Options) != 3 {
		t.Errorf("options = %+v", q.Fields[0].Options)
	}
}

func TestParsePaneQuestion_NoSelector(t *testing.T) {
	if q := parsePaneQuestion("just a normal shell\n❯ typing here"); q != nil {
		t.Errorf("want nil (no selector footer); got %+v", q)
	}
}

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
	if strings.Join(got, ",") != "Apple,Banana" {
		t.Errorf("options = %v; scrollback leaked?", got)
	}
}

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

// withHook points the adapter at a fresh pending dir and drops a hook payload.
func withHook(t *testing.T, a *Adapter, sid, payload string) {
	t.Helper()
	a.pendingDir = t.TempDir()
	if err := os.WriteFile(filepath.Join(a.pendingDir, sid+".json"), []byte(payload), 0o644); err != nil {
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

func TestHookQuestion_Pending(t *testing.T) {
	a := newAdapter(fakeHome(t))
	tp := filepath.Join(t.TempDir(), "t.jsonl")
	os.WriteFile(tp, []byte(`{"type":"user"}`+"\n"), 0o644)
	withHook(t, a, "sess-A", fmt.Sprintf(hookPayloadJSON, tp))
	q := a.hookQuestion("sess-A")
	if q == nil || q.ID != "toolu_ABC" || len(q.Fields) != 1 {
		t.Fatalf("q = %+v", q)
	}
	f := q.Fields[0]
	if f.Prompt != "Deploy?" || len(f.Options) != 2 || f.Options[1].Value != "Production" || f.Options[0].Detail != "to staging" {
		t.Errorf("field = %+v", f)
	}
}

func TestHookQuestion_AnsweredClears(t *testing.T) {
	a := newAdapter(fakeHome(t))
	tp := filepath.Join(t.TempDir(), "t.jsonl")
	os.WriteFile(tp, []byte(`{"type":"user","tool_use_id":"toolu_ABC"}`+"\n"), 0o644)
	withHook(t, a, "sess-A", fmt.Sprintf(hookPayloadJSON, tp))
	if q := a.hookQuestion("sess-A"); q != nil {
		t.Errorf("want nil (answered); got %+v", q)
	}
	if _, err := os.Stat(filepath.Join(a.pendingDir, "sess-A.json")); !os.IsNotExist(err) {
		t.Error("stale hook file should have been removed")
	}
}

func TestState_AwaitingUserFromHook(t *testing.T) {
	a := newAdapter(fakeHome(t))
	tp := filepath.Join(t.TempDir(), "t.jsonl")
	os.WriteFile(tp, []byte(`{}`+"\n"), 0o644)
	withHook(t, a, "sess-A", fmt.Sprintf(hookPayloadJSON, tp))
	st, err := a.State(context.Background(), sessA(), &fakeTmux{})
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != source.StatusAwaitingUser || len(st.Pending) != 1 {
		t.Errorf("state = %+v", st)
	}
}

func TestAct_UsesHookQuestion(t *testing.T) {
	a := newAdapter(fakeHome(t))
	f := &fakeTmux{live: map[string]string{"sess-A": "work:2.1"}}
	tp := filepath.Join(t.TempDir(), "t.jsonl")
	os.WriteFile(tp, []byte(`{}`+"\n"), 0o644)
	withHook(t, a, "sess-A", fmt.Sprintf(hookPayloadJSON, tp))
	_, err := a.Act(context.Background(), sessA(), source.Action{
		Name: "answer_questionnaire",
		Args: map[string]any{"answers": map[string]any{"Deploy": "Production"}},
	}, f)
	if err != nil {
		t.Fatal(err)
	}
	if !f.sawSeq("send-keys", "-t", "work:2.1", "-l", "2") { // Production = option 2
		t.Errorf("did not send digit 2: %v", f.calls)
	}
}

func TestState_AwaitingUserFromPane(t *testing.T) {
	a := newAdapter(fakeHome(t))
	f := &fakeTmux{live: map[string]string{"sess-A": "work:2.1"}, capture: paneSingleSelect}
	st, err := a.State(context.Background(), sessA(), f)
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

func TestAct_SingleSelect(t *testing.T) {
	a := newAdapter(fakeHome(t))
	f := &fakeTmux{live: map[string]string{"sess-A": "work:2.1"}, capture: paneSingleSelect}
	res, err := a.Act(context.Background(), sessA(), source.Action{
		Name: "answer_questionnaire",
		Args: map[string]any{"answers": map[string]any{"answer": "Banana"}},
	}, f)
	if err != nil {
		t.Fatal(err)
	}
	if res == "" {
		t.Error("empty result")
	}
	if !f.sawSeq("send-keys", "-t", "work:2.1", "-l", "2") {
		t.Errorf("did not send digit 2 to the pane: %v", f.calls)
	}
}

func TestAct_MultiSelect(t *testing.T) {
	a := newAdapter(fakeHome(t))
	f := &fakeTmux{live: map[string]string{"sess-A": "work:2.1"}, capture: paneMultiSelect}
	_, err := a.Act(context.Background(), sessA(), source.Action{
		Name: "answer_questionnaire",
		Args: map[string]any{"answers": map[string]any{"answer": []any{"Cheese", "Mushroom"}}},
	}, f)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range [][]string{{"-l", "1"}, {"-l", "3"}, {"Right"}, {"-l", "1"}} {
		if !f.sawSeq(append([]string{"send-keys", "-t", "work:2.1"}, want...)...) {
			t.Errorf("missing keystroke %v in %v", want, f.calls)
		}
	}
}

func TestAct_NoPaneErrors(t *testing.T) {
	a := newAdapter(fakeHome(t))
	if _, err := a.Act(context.Background(), sessA(), source.Action{
		Name: "answer_questionnaire",
		Args: map[string]any{"answers": map[string]any{"answer": "a"}},
	}, &fakeTmux{}); err == nil {
		t.Fatal("expected error when session not live in a pane")
	}
}

func TestAct_NoQuestionErrors(t *testing.T) {
	a := newAdapter(fakeHome(t))
	f := &fakeTmux{live: map[string]string{"sess-A": "work:2.1"}, capture: "just a shell prompt\n❯ "}
	if _, err := a.Act(context.Background(), sessA(), source.Action{
		Name: "answer_questionnaire",
		Args: map[string]any{"answers": map[string]any{"answer": "a"}},
	}, f); err == nil {
		t.Fatal("expected error when no question is on screen")
	}
}
