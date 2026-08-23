// Package scratchpad is the user-facing work-list: durable tasks/todos the
// concierge (and the PWA) can add, list, and complete. It is the INBOUND path
// to the cue pipeline — the mirror of the LLM generator's outbound proposals.
//
// Design line (see plan/plan.md): LLM proposes, deterministic layer validates.
// A task enters only through Add, which normalizes + dedupes; the model never
// writes rows directly. A todo is a Task whose Target is empty — it stays in
// the list until completed or promoted to real work (which reuses the cue
// release pipeline).
package scratchpad

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
)

// TaskKind distinguishes user todos from generator-proposed work.
type TaskKind string

const (
	KindTodo    TaskKind = "todo"    // user-facing, added via conversation or PWA
	KindCue     TaskKind = "cue"     // generator-proposed (the existing cue path)
	KindCommand TaskKind = "command" // a terminal command to run + watch (slice 3)
)

// Task is one scratchpad entry. It carries the cue shape so a promoted todo can
// flow straight into the release pipeline without translation. Status is
// carried on the row (the store orders/filters by it); List returns it filled in.
type Task struct {
	Status Status // set by the store on reads; zero when freshly constructed
	ID        string
	DedupeKey string
	Prompt    string // what the work is (the todo text, or the command)
	Kind      TaskKind
	Priority  int
	CreatedAt int64 // ns

	// Scheduling: RunAt > 0 makes a task eligible for release only from that
	// instant (a reminder / scheduled todo). Cron is a repeat spec — when set,
	// completing the task re-arms it at the next occurrence instead of closing.
	RunAt int64  // ns; 0 = unscheduled
	Cron  string

	// Target: where promoted work goes. Empty for a plain todo.
	Target Target
}

// Target mirrors cue.Target (kept local so the scratchpad store doesn't import
// the scheduler; promotion copies across).
type Target struct {
	Source    string
	SessionID string
	Spawn     bool
	SpawnDir  string
}

// Status is a task's lifecycle state.
type Status string

const (
	StatusPending   Status = "pending"   // waiting (todo, or gated by schedule/caps)
	StatusInflight  Status = "inflight"  // released to a session
	StatusDone      Status = "done"      // completed
	StatusFailed    Status = "failed"
	StatusCompleted Status = "completed" // user marked a todo done (terminal, listable)
)

// Store is the durable scratchpad. *store.PG satisfies it; tests use fakes.
type Store interface {
	// Add inserts a new task (pending). Returns the stored task, or nil if a
	// live task with the same non-empty DedupeKey already exists (no-op dedupe).
	Add(ctx context.Context, t Task) (*Task, error)
	// List returns tasks matching kinds (empty = all), open ones first:
	// pending then inflight, newest-first; closed (done/failed/completed) after.
	List(ctx context.Context, kinds ...TaskKind) ([]Task, error)
	// Complete marks a task completed/done by id (status-guarded). Returns false
	// if the id is unknown or already terminal.
	Complete(ctx context.Context, id string, done bool) (bool, error)
	// Note appends a free-form note to a task's record (visible in List output
	// via the PWA; not part of the release pipeline).
	Note(ctx context.Context, id, note string) (bool, error)
	// Get returns one task by id.
	Get(ctx context.Context, id string) (*Task, error)
}

// Now is the clock seam for tests.
var Now = time.Now

// NextRun reports the next occurrence of cron at or after t, as ns. It is a
// thin wrapper over ParseCron so callers have one entry point; 0 when the spec
// has no future occurrence within the horizon.
func NextRun(cron string, t time.Time) (int64, error) {
	spec, err := ParseCron(cron)
	if err != nil {
		return 0, err
	}
	nxt, ok := spec.Next(t)
	if !ok {
		return 0, nil
	}
	return nxt.UnixNano(), nil
}

// Normalize tidies a proposed task before it hits the store: trims the prompt,
// lowercases + trims the cron spec, and derives a DedupeKey from the prompt
// when one wasn't supplied (so re-adding the same todo is a no-op).
func Normalize(t Task) Task {
	t.Prompt = strings.TrimSpace(t.Prompt)
	t.Cron = strings.ToLower(strings.TrimSpace(t.Cron))
	if t.ID == "" && t.Prompt != "" {
		t.ID = uuid.NewString()
	}
	if t.CreatedAt == 0 {
		t.CreatedAt = time.Now().UnixNano()
	}
	if t.DedupeKey == "" && t.Prompt != "" {
		t.DedupeKey = string(t.Kind) + "|" + t.Prompt
	}
	return t
}
