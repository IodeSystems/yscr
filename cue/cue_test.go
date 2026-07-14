package cue

import (
	"testing"

	"github.com/iodesystems/yscr/source"
)

func st(src, id string, status source.Status) source.State {
	return source.State{Ref: source.SessionRef{Source: src, ID: id}, Status: status}
}

func existing(id string, src string, prio int, created int64) Task {
	return Task{ID: id, Priority: prio, CreatedAt: created, Target: Target{Source: src, SessionID: id}}
}

// released returns the ids of tasks Plan chose to release.
func released(ds []Decision) map[string]bool {
	m := map[string]bool{}
	for _, d := range ds {
		if d.Release {
			m[d.Task.ID] = true
		}
	}
	return m
}

func TestPlan_StatusGate(t *testing.T) {
	fleet := []source.State{
		st("cc", "idle1", source.StatusIdle),
		st("cc", "done1", source.StatusDone),
		st("cc", "await1", source.StatusAwaitingUser),
		st("cc", "run1", source.StatusRunning),
		st("cc", "block1", source.StatusBlocked),
		st("cc", "fail1", source.StatusFailed),
	}
	tasks := []Task{
		existing("idle1", "cc", 0, 1),
		existing("done1", "cc", 0, 2),
		existing("await1", "cc", 0, 3),
		existing("run1", "cc", 0, 4),
		existing("block1", "cc", 0, 5),
		existing("fail1", "cc", 0, 6),
	}
	got := released(Plan(tasks, fleet, nil, Caps{}, nil))
	want := map[string]bool{"idle1": true, "done1": true, "await1": true}
	for id := range want {
		if !got[id] {
			t.Errorf("expected %s released", id)
		}
	}
	for _, id := range []string{"run1", "block1", "fail1"} {
		if got[id] {
			t.Errorf("did not expect %s released (status should hold)", id)
		}
	}
}

func TestPlan_PerSessionCap(t *testing.T) {
	fleet := []source.State{st("cc", "s1", source.StatusIdle)}
	tasks := []Task{
		{ID: "a", Priority: 1, Target: Target{Source: "cc", SessionID: "s1"}},
		{ID: "b", Priority: 1, Target: Target{Source: "cc", SessionID: "s1"}},
	}
	// Default per-session cap = 1: exactly one of the two releases.
	got := released(Plan(tasks, fleet, nil, Caps{}, nil))
	if len(got) != 1 {
		t.Fatalf("per-session default cap 1: got %d releases, want 1", len(got))
	}
	// Cap 2: both release.
	got = released(Plan(tasks, fleet, nil, Caps{PerSession: 2}, nil))
	if len(got) != 2 {
		t.Fatalf("per-session cap 2: got %d releases, want 2", len(got))
	}
	// Already one in-flight against s1 → default cap 1 holds both.
	got = released(Plan(tasks, fleet, map[string]int{"cc/s1": 1}, Caps{}, nil))
	if len(got) != 0 {
		t.Fatalf("in-flight seeds the cap: got %d releases, want 0", len(got))
	}
}

func TestPlan_GlobalCapHonorsPriority(t *testing.T) {
	fleet := []source.State{
		st("cc", "s1", source.StatusIdle),
		st("cc", "s2", source.StatusIdle),
		st("cc", "s3", source.StatusIdle),
	}
	tasks := []Task{
		existing("s1", "cc", 1, 1),
		existing("s2", "cc", 9, 1), // highest priority
		existing("s3", "cc", 5, 1),
	}
	got := released(Plan(tasks, fleet, nil, Caps{Global: 2}, nil))
	if len(got) != 2 {
		t.Fatalf("global cap 2: got %d releases, want 2", len(got))
	}
	if !got["s2"] || !got["s3"] {
		t.Errorf("global cap should release the two highest priorities (s2,s3), got %v", got)
	}
	if got["s1"] {
		t.Errorf("lowest priority s1 should be held under the global cap")
	}
}

func TestPlan_SpawnCap(t *testing.T) {
	spawn := func(id string) Task { return Task{ID: id, Target: Target{Source: "cc", Spawn: true, SpawnDir: "/x"}} }
	tasks := []Task{spawn("a"), spawn("b"), spawn("c")}
	got := released(Plan(tasks, nil, nil, Caps{MaxSpawns: 2}, nil))
	if len(got) != 2 {
		t.Fatalf("spawn cap 2: got %d releases, want 2", len(got))
	}
	// One spawn already in flight → only one more of the three releases.
	got = released(Plan(tasks, nil, map[string]int{spawnKey: 1}, Caps{MaxSpawns: 2}, nil))
	if len(got) != 1 {
		t.Fatalf("spawn cap 2 with 1 in-flight: got %d releases, want 1", len(got))
	}
}

func TestPlan_UnknownSessionHeld(t *testing.T) {
	tasks := []Task{existing("ghost", "cc", 0, 1)}
	ds := Plan(tasks, nil, nil, Caps{}, nil) // empty fleet
	if len(ds) != 1 || ds[0].Release {
		t.Fatalf("unknown session should hold: %+v", ds)
	}
	if ds[0].Reason == "" {
		t.Error("held decision should carry a reason")
	}
}

func TestPlan_CustomReleasableWidensToRunning(t *testing.T) {
	fleet := []source.State{st("cc", "run1", source.StatusRunning)}
	tasks := []Task{existing("run1", "cc", 0, 1)}
	relax := map[source.Status]bool{source.StatusRunning: true}
	got := released(Plan(tasks, fleet, nil, Caps{}, relax))
	if !got["run1"] {
		t.Error("widened releasable set should allow pushing onto a running session")
	}
}
