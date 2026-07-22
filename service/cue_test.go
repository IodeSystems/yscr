package service

import (
	"context"
	"testing"
	"time"

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
	pending   []cue.Task
	inflight  []cue.Task
	rows      []store.InflightRow
	marked    []string // ids MarkInflight'd
	seenBusy  []string // ids MarkSeenBusy'd
	done      []string // ids MarkDone'd
	failed    []string // ids MarkFailed'd
	lastRunID string   // run_session passed to the last MarkInflight
}

func (f *fakeCueStore) PendingTasks(context.Context) ([]cue.Task, error)  { return f.pending, nil }
func (f *fakeCueStore) InflightTasks(context.Context) ([]cue.Task, error) { return f.inflight, nil }
func (f *fakeCueStore) InflightRows(context.Context) ([]store.InflightRow, error) {
	return f.rows, nil
}
func (f *fakeCueStore) MarkInflight(_ context.Context, id, runSession string, _ int64) (bool, error) {
	f.marked = append(f.marked, id)
	f.lastRunID = runSession
	return true, nil
}
func (f *fakeCueStore) MarkSeenBusy(_ context.Context, id string) error {
	f.seenBusy = append(f.seenBusy, id)
	return nil
}
func (f *fakeCueStore) MarkDone(_ context.Context, id string, _ int64) (bool, error) {
	f.done = append(f.done, id)
	return true, nil
}
func (f *fakeCueStore) MarkFailed(_ context.Context, id string, _ int64) (bool, error) {
	f.failed = append(f.failed, id)
	return true, nil
}

type fakeCueSource struct {
	id     string
	posts  []string // "id:msg"
	spawns []source.SpawnSpec
}

func (f *fakeCueSource) ID() string                                        { return f.id }
func (f *fakeCueSource) List(context.Context) ([]source.SessionRef, error) { return nil, nil }
func (f *fakeCueSource) State(context.Context, string) (source.State, error) {
	return source.State{}, nil
}
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
	if fs.lastRunID != "spawned" {
		t.Errorf("spawn must record the new session id as run_session, got %q", fs.lastRunID)
	}
}

// ── reconcile (phase 3.5: completion detection) ─────────────────────

func inflightRow(id, src, sess string, seenBusy bool, releasedAt int64) store.InflightRow {
	return store.InflightRow{ID: id, Source: src, RunSession: sess, SeenBusy: seenBusy, ReleasedAt: releasedAt}
}
func statusState(src, id string, s source.Status) source.State {
	return source.State{Ref: source.SessionRef{Source: src, ID: id}, Status: s}
}
func recRunner(fs *fakeCueStore, ttl time.Duration) *cueRunner {
	return &cueRunner{store: fs, sources: map[string]source.Source{}, notify: func(string, string) {}, completionTTL: ttl}
}

func TestCueReconcile_LatchThenComplete(t *testing.T) {
	fs := &fakeCueStore{rows: []store.InflightRow{inflightRow("t1", "cc", "s1", false, 0)}}
	r := recRunner(fs, 0)
	// Running + not-yet-latched → latch seen_busy, not done.
	r.reconcile(context.Background(), []source.State{statusState("cc", "s1", source.StatusRunning)})
	if len(fs.seenBusy) != 1 || len(fs.done) != 0 {
		t.Fatalf("running should latch seen_busy only: seenBusy=%v done=%v", fs.seenBusy, fs.done)
	}
	// Latched + now free → done.
	fs.rows[0].SeenBusy = true
	r.reconcile(context.Background(), []source.State{statusState("cc", "s1", source.StatusIdle)})
	if len(fs.done) != 1 || fs.done[0] != "t1" {
		t.Fatalf("latched+free should complete: done=%v", fs.done)
	}
}

func TestCueReconcile_Failed(t *testing.T) {
	fs := &fakeCueStore{rows: []store.InflightRow{inflightRow("t1", "cc", "s1", true, 0)}}
	recRunner(fs, 0).reconcile(context.Background(), []source.State{statusState("cc", "s1", source.StatusFailed)})
	if len(fs.failed) != 1 {
		t.Fatalf("failed session should MarkFailed: %v", fs.failed)
	}
}

func TestCueReconcile_GoneAfterBusy(t *testing.T) {
	fs := &fakeCueStore{rows: []store.InflightRow{inflightRow("t1", "cc", "s1", true, 0)}}
	recRunner(fs, 0).reconcile(context.Background(), nil) // session absent from fleet
	if len(fs.done) != 1 {
		t.Fatalf("session gone after being busy should complete: %v", fs.done)
	}
}

func TestCueReconcile_NotPickedUpHolds(t *testing.T) {
	// Freshly dispatched, session still idle (not picked up), TTL far off → hold.
	fs := &fakeCueStore{rows: []store.InflightRow{inflightRow("t1", "cc", "s1", false, time.Now().UnixNano())}}
	recRunner(fs, time.Hour).reconcile(context.Background(), []source.State{statusState("cc", "s1", source.StatusIdle)})
	if len(fs.done) != 0 || len(fs.failed) != 0 || len(fs.seenBusy) != 0 {
		t.Fatalf("un-picked-up task must hold: done=%v failed=%v seenBusy=%v", fs.done, fs.failed, fs.seenBusy)
	}
}

func TestCueReconcile_TTLBackstop(t *testing.T) {
	old := time.Now().Add(-2 * time.Hour).UnixNano()
	fs := &fakeCueStore{rows: []store.InflightRow{inflightRow("t1", "cc", "s1", false, old)}}
	recRunner(fs, time.Hour).reconcile(context.Background(), []source.State{statusState("cc", "s1", source.StatusIdle)})
	if len(fs.done) != 1 {
		t.Fatalf("TTL backstop should reclaim a stuck task: done=%v", fs.done)
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
