// Package cue is the outbound task scheduler — the mirror of the concierge's
// inbound coalescing dispatch. It manages the flow of proposed work TO fleet
// sessions given the fleet is "rarely truly idle", so wait-for-idle is not a
// viable gate.
//
// This file is PHASE 1: the data model + the deterministic release gate (Plan).
// Plan is a pure function — (cued tasks, fleet snapshot, in-flight counts, caps)
// → release/hold decisions, no side effects — so the push-vs-hold policy is
// cheap, predictable, and fully testable with no LLM in the hot path.
//
// Later phases (see plan/plan.md): a cue store (Postgres), an LLM generator tick
// that proposes Tasks from fleet state + standing goals, and a release loop that
// executes RELEASE decisions autonomously (Post/Spawn) behind safety rails.
package cue

import (
	"sort"

	"github.com/iodesystems/yscr/source"
)

// Task is one unit of work the generator proposed for the fleet.
type Task struct {
	ID        string // stable unique id
	DedupeKey string // generator identity; two tasks with the same key are the same work
	Prompt    string // what to hand the target session
	Priority  int    // higher is scheduled first
	CreatedAt int64  // ns; tie-break (older first) so ordering is stable
	Target    Target
}

// Target says where a task should go: an existing session (Source+SessionID,
// dispatched via source.Source.Post) or a new one (Spawn, via source.Spawner).
type Target struct {
	Source    string // plugin id — "autowork" | "claude-code" | "openai"
	SessionID string // existing session; empty + Spawn ⇒ new session
	Spawn     bool   // request a new session instead of routing to an existing one
	SpawnDir  string // cwd for a spawn (claude-code), optional
}

// spawnKey is the shared in-flight bucket for spawn tasks (capacity is tracked in
// aggregate, not per-dir, in phase 1).
const spawnKey = "\x00spawn"

// key identifies the in-flight bucket a task counts against.
func (t Target) key() string {
	if t.Spawn {
		return spawnKey
	}
	return t.Source + "/" + t.SessionID
}

// Caps bound how much work is in flight. A zero field means "no limit", except
// PerSession, where zero means the default of 1 (one task at a time per session).
type Caps struct {
	PerSession int // max in-flight tasks per target session (default 1)
	Global     int // max in-flight fleet-wide (0 = unlimited)
	MaxSpawns  int // max in-flight spawns (0 = unlimited)
}

// DefaultReleasable is the set of session statuses that accept a new task. It is
// the set the scheduling decision was specified against: a session that is idle,
// done, or awaiting user input can take work; running/blocked/failed hold. Widen
// it (include StatusRunning) to pile work onto active sessions up to the cap.
var DefaultReleasable = map[source.Status]bool{
	source.StatusIdle:         true,
	source.StatusDone:         true,
	source.StatusAwaitingUser: true,
}

// Decision is Plan's verdict for one task: release it now, or hold with a reason
// (kept for logging / the phase-3 dry-run).
type Decision struct {
	Task    Task
	Release bool
	Reason  string // why it was held; "" when released
}

// Plan decides, for each cued task, release vs hold under the status + capacity
// gate. Pure: inputs are snapshots and nothing is mutated.
//
//   - fleet:    current per-session state (from the fleet watcher).
//   - inflight: tasks already released and not yet done, keyed by Target.key()
//     (spawns counted under the shared spawn bucket). Callers build this from the
//     cue store; Plan treats it as a starting count and adds its own releases so a
//     single Plan call never exceeds a cap.
//   - releasable: nil ⇒ DefaultReleasable.
//
// Tasks are evaluated highest-priority-first (older first on a tie), and the
// decisions are returned in that evaluation order.
func Plan(tasks []Task, fleet []source.State, inflight map[string]int, caps Caps, releasable map[source.Status]bool) []Decision {
	if releasable == nil {
		releasable = DefaultReleasable
	}

	byKey := make(map[string]source.State, len(fleet))
	for _, st := range fleet {
		byKey[st.Ref.Source+"/"+st.Ref.ID] = st
	}

	// Working counters seeded from the in-flight snapshot; Plan's own releases
	// increment them so caps hold within one call.
	used := make(map[string]int, len(inflight))
	global := 0
	for k, n := range inflight {
		used[k] = n
		global += n
	}

	ordered := append([]Task(nil), tasks...)
	sort.SliceStable(ordered, func(i, j int) bool {
		a, b := ordered[i], ordered[j]
		if a.Priority != b.Priority {
			return a.Priority > b.Priority
		}
		if a.CreatedAt != b.CreatedAt {
			return a.CreatedAt < b.CreatedAt
		}
		return a.ID < b.ID
	})

	out := make([]Decision, 0, len(ordered))
	hold := func(t Task, reason string) { out = append(out, Decision{Task: t, Reason: reason}) }
	release := func(t Task, k string) {
		out = append(out, Decision{Task: t, Release: true})
		used[k]++
		global++
	}

	for _, t := range ordered {
		k := t.Target.key()

		if caps.Global > 0 && global >= caps.Global {
			hold(t, "global cap reached")
			continue
		}

		if t.Target.Spawn {
			if caps.MaxSpawns > 0 && used[spawnKey] >= caps.MaxSpawns {
				hold(t, "spawn cap reached")
				continue
			}
			release(t, k) // a spawn creates the session, so there is no status to check
			continue
		}

		st, ok := byKey[k]
		if !ok {
			hold(t, "target session not in fleet")
			continue
		}
		if !releasable[st.Status] {
			hold(t, "status "+string(st.Status)+" not releasable")
			continue
		}
		perCap := caps.PerSession
		if perCap <= 0 {
			perCap = 1
		}
		if used[k] >= perCap {
			hold(t, "per-session cap reached")
			continue
		}
		release(t, k)
	}
	return out
}
