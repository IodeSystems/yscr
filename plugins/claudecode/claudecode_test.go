package claudecode

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iodesystems/yscr/source"
)

// fakeTmux records tmux invocations. has-session always fails (nothing runs
// under us in tests), so the dormant/resume paths exercise.
type fakeTmux struct{ calls [][]string }

func (f *fakeTmux) run(_ context.Context, _ string, args ...string) (string, error) {
	f.calls = append(f.calls, args)
	switch args[0] {
	case "has-session":
		return "", fmt.Errorf("no session")
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
	write(filepath.Join(home, "sessions", "1.json"), `{"sessionId":"sess-A","cwd":"/repo/alpha","status":"shell","updatedAt":200}`)
	write(filepath.Join(home, "sessions", "2.json"), `{"sessionId":"sess-B","cwd":"/repo/beta","status":"shell","updatedAt":100}`)
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
