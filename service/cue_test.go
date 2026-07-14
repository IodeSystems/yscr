package service

import (
	"context"
	"testing"

	"github.com/iodesystems/yscr/config"
	"github.com/iodesystems/yscr/cue"
	"github.com/iodesystems/yscr/source"
	"github.com/iodesystems/yscr/store"
)

func TestNewCueRunner_Gate(t *testing.T) {
	pg := &store.PG{} // zero value ok — newCueRunner never touches the pool
	noop := func(string, string) {}
	if r := newCueRunner(config.CueConfig{Enabled: false}, pg, nil, noop); r != nil {
		t.Error("disabled cue must yield a nil runner (kill-switch)")
	}
	if r := newCueRunner(config.CueConfig{Enabled: true}, nil, nil, noop); r != nil {
		t.Error("no durable store must yield a nil runner")
	}
	r := newCueRunner(config.CueConfig{Enabled: true, PerSessionCap: 1}, pg, nil, noop)
	if r == nil {
		t.Fatal("enabled + store must yield a runner")
	}
	if !r.dryRun {
		t.Error("DryRun defaults on when unset — an enabled cue must not act live by default")
	}
}

type fakeCueStore struct {
	pending  []cue.Task
	inflight []cue.Task
	marked   []string
}

func (f *fakeCueStore) PendingTasks(context.Context) ([]cue.Task, error)  { return f.pending, nil }
func (f *fakeCueStore) InflightTasks(context.Context) ([]cue.Task, error) { return f.inflight, nil }
func (f *fakeCueStore) MarkInflight(_ context.Context, id string, _ int64) (bool, error) {
	f.marked = append(f.marked, id)
	return true, nil
}

type fakeCueSource struct {
	id     string
	posts  []string // "id:msg"
	spawns []source.SpawnSpec
}

func (f *fakeCueSource) ID() string                                             { return f.id }
func (f *fakeCueSource) List(context.Context) ([]source.SessionRef, error)      { return nil, nil }
func (f *fakeCueSource) State(context.Context, string) (source.State, error)    { return source.State{}, nil }
func (f *fakeCueSource) Observe(context.Context, string) (<-chan source.Event, error) {
	return nil, nil
}
func (f *fakeCueSource) Post(_ context.Context, id, msg string) error {
	f.posts = append(f.posts, id+":"+msg)
	return nil
}
func (f *fakeCueSource) Spawn(_ context.Context, spec source.SpawnSpec) (source.SessionRef, error) {
	f.spawns = append(f.spawns, spec)
	return source.SessionRef{Source: f.id, ID: "spawned"}, nil
}

func existingTask(id, src, sess string) cue.Task {
	return cue.Task{ID: id, Prompt: "do " + id, Target: cue.Target{Source: src, SessionID: sess}}
}
func idleState(src, id string) source.State {
	return source.State{Ref: source.SessionRef{Source: src, ID: id}, Status: source.StatusIdle}
}

func runner(fs *fakeCueStore, src *fakeCueSource, dryRun bool, caps cue.Caps, maxPerHour int) *cueRunner {
	return &cueRunner{
		store:      fs,
		sources:    map[string]source.Source{src.id: src},
		caps:       caps,
		dryRun:     dryRun,
		maxPerHour: maxPerHour,
		notify:     func(string, string) {},
	}
}

func TestCueRelease_DryRunActsNothing(t *testing.T) {
	fs := &fakeCueStore{pending: []cue.Task{existingTask("t1", "cc", "s1")}}
	src := &fakeCueSource{id: "cc"}
	runner(fs, src, true, cue.Caps{}, 0).release(context.Background(), []source.State{idleState("cc", "s1")})
	if len(src.posts) != 0 || len(fs.marked) != 0 {
		t.Fatalf("dry-run must not act: posts=%v marked=%v", src.posts, fs.marked)
	}
}

func TestCueRelease_LivePostsAndMarks(t *testing.T) {
	fs := &fakeCueStore{pending: []cue.Task{existingTask("t1", "cc", "s1")}}
	src := &fakeCueSource{id: "cc"}
	runner(fs, src, false, cue.Caps{}, 0).release(context.Background(), []source.State{idleState("cc", "s1")})
	if len(src.posts) != 1 || src.posts[0] != "s1:do t1" {
		t.Fatalf("expected Post to s1, got %v", src.posts)
	}
	if len(fs.marked) != 1 || fs.marked[0] != "t1" {
		t.Fatalf("expected t1 marked inflight, got %v", fs.marked)
	}
}

func TestCueRelease_HeldStatusNotDispatched(t *testing.T) {
	fs := &fakeCueStore{pending: []cue.Task{existingTask("t1", "cc", "s1")}}
	src := &fakeCueSource{id: "cc"}
	running := source.State{Ref: source.SessionRef{Source: "cc", ID: "s1"}, Status: source.StatusRunning}
	runner(fs, src, false, cue.Caps{}, 0).release(context.Background(), []source.State{running})
	if len(src.posts) != 0 {
		t.Fatalf("running session is not releasable; expected no post, got %v", src.posts)
	}
}

func TestCueRelease_SpawnLive(t *testing.T) {
	spawn := cue.Task{ID: "sp", Prompt: "start it", Target: cue.Target{Source: "cc", Spawn: true, SpawnDir: "/w"}}
	fs := &fakeCueStore{pending: []cue.Task{spawn}}
	src := &fakeCueSource{id: "cc"}
	runner(fs, src, false, cue.Caps{}, 0).release(context.Background(), nil)
	if len(src.spawns) != 1 || src.spawns[0].Prompt != "start it" || src.spawns[0].Dir != "/w" {
		t.Fatalf("expected one spawn with prompt+dir, got %v", src.spawns)
	}
	if len(fs.marked) != 1 {
		t.Fatalf("spawn should be marked inflight, got %v", fs.marked)
	}
}

func TestCueRelease_MaxPerHourCaps(t *testing.T) {
	fs := &fakeCueStore{pending: []cue.Task{
		existingTask("t1", "cc", "s1"),
		existingTask("t2", "cc", "s2"),
	}}
	src := &fakeCueSource{id: "cc"}
	fleet := []source.State{idleState("cc", "s1"), idleState("cc", "s2")}
	runner(fs, src, false, cue.Caps{}, 1).release(context.Background(), fleet)
	if len(src.posts) != 1 {
		t.Fatalf("maxPerHour=1 should dispatch exactly one, got %v", src.posts)
	}
}
