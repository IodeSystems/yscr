package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/yscr/cue"
	"github.com/iodesystems/yscr/scratchpad"
)

// PG is a Postgres-backed agent.Store (durable concierge conversation) that
// also persists web-push subscriptions. One isolated db/schema per the yscr
// role. Tables land in the role's default schema (search_path=yscr).
type PG struct {
	pool *pgxpool.Pool
}

const pgSchema = `
CREATE TABLE IF NOT EXISTS entries (
	session_id     text   NOT NULL,
	id             text   NOT NULL,
	kind           text   NOT NULL,
	content        text   NOT NULL,
	tool_call_id   text   NOT NULL DEFAULT '',
	tool_name      text   NOT NULL DEFAULT '',
	tag            text   NOT NULL DEFAULT '',
	origin         text   NOT NULL DEFAULT '',
	created_at     bigint NOT NULL,
	compacted_into text,
	PRIMARY KEY (session_id, id)
);
CREATE INDEX IF NOT EXISTS entries_context ON entries (session_id, created_at, id)
	WHERE compacted_into IS NULL;

CREATE TABLE IF NOT EXISTS push_subscriptions (
	endpoint   text   PRIMARY KEY,
	p256dh     text   NOT NULL,
	auth       text   NOT NULL,
	created_at bigint NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS cue_tasks (
	id             text   PRIMARY KEY,
	dedupe_key     text   NOT NULL DEFAULT '',
	prompt         text   NOT NULL,
	priority       int    NOT NULL DEFAULT 0,
	target_source  text   NOT NULL,
	target_session text   NOT NULL DEFAULT '',
	target_spawn   bool   NOT NULL DEFAULT false,
	target_dir     text   NOT NULL DEFAULT '',
	status         text   NOT NULL DEFAULT 'pending', -- pending | inflight | done | failed
	run_session    text   NOT NULL DEFAULT '',        -- session the task actually runs in (spawned id for spawns)
	seen_busy      bool   NOT NULL DEFAULT false,      -- reconciler latch: went busy after dispatch
	created_at     bigint NOT NULL,
	released_at    bigint NOT NULL DEFAULT 0,
	done_at        bigint NOT NULL DEFAULT 0
);
-- Migrate existing deployments (phase 3.5 columns).
ALTER TABLE cue_tasks ADD COLUMN IF NOT EXISTS run_session text NOT NULL DEFAULT '';
ALTER TABLE cue_tasks ADD COLUMN IF NOT EXISTS seen_busy   bool NOT NULL DEFAULT false;
CREATE INDEX IF NOT EXISTS cue_pending ON cue_tasks (priority DESC, created_at) WHERE status='pending';
-- One live task per dedupe identity: dedupe_key='' opts out (partial-index NULLs).
CREATE UNIQUE INDEX IF NOT EXISTS cue_dedupe_live ON cue_tasks (dedupe_key)
	WHERE status IN ('pending','inflight') AND dedupe_key <> '';`

// NewPG connects, applies the schema, and returns the store.
func NewPG(ctx context.Context, dsn string) (*PG, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("yscr/store: connect: %w", err)
	}
	for _, stmt := range []string{pgSchema, scratchpadSchema} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			pool.Close()
			return nil, fmt.Errorf("yscr/store: migrate: %w", err)
		}
	}
	return &PG{pool: pool}, nil
}

func (p *PG) Close() { p.pool.Close() }

// ── agent.Store ─────────────────────────────────────────────────────

// ClaimPending: no external inbox (user messages are Injected then a Turn
// runs synchronously), so nothing is pending mid-turn.
func (p *PG) ClaimPending(_ context.Context, _ string, _ int64) (int, error) { return 0, nil }

func (p *PG) Append(ctx context.Context, sessionID string, e agent.Entry) error {
	_, err := p.pool.Exec(ctx,
		`INSERT INTO entries (session_id, id, kind, content, tool_call_id, tool_name, tag, origin, created_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		 ON CONFLICT (session_id, id) DO NOTHING`,
		sessionID, e.ID, string(e.Kind), e.Content, e.ToolCallID, e.ToolName, e.Tag, e.Origin, e.CreatedAt)
	return err
}

func (p *PG) Context(ctx context.Context, sessionID string) ([]agent.Entry, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT id, kind, content, tool_call_id, tool_name, tag, origin, created_at
		 FROM entries WHERE session_id=$1 AND compacted_into IS NULL
		 ORDER BY created_at, id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []agent.Entry
	for rows.Next() {
		var e agent.Entry
		var kind string
		if err := rows.Scan(&e.ID, &kind, &e.Content, &e.ToolCallID, &e.ToolName, &e.Tag, &e.Origin, &e.CreatedAt); err != nil {
			return nil, err
		}
		e.Kind = agent.EntryKind(kind)
		out = append(out, e)
	}
	return out, rows.Err()
}

func (p *PG) Compact(ctx context.Context, sessionID string, c agent.Compaction) error {
	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		for _, e := range c.Subsumes {
			if _, err := tx.Exec(ctx,
				`UPDATE entries SET compacted_into=$1 WHERE session_id=$2 AND id=$3`,
				c.Marker.ID, sessionID, e.ID); err != nil {
				return err
			}
		}
		m := c.Marker
		_, err := tx.Exec(ctx,
			`INSERT INTO entries (session_id, id, kind, content, created_at)
			 VALUES ($1,$2,$3,$4,$5) ON CONFLICT (session_id, id) DO NOTHING`,
			sessionID, m.ID, string(m.Kind), m.Content, m.CreatedAt)
		return err
	})
}

// ── push subscriptions ──────────────────────────────────────────────

// PushSub is one stored web-push subscription (matches webpush.Subscription).
type PushSub struct {
	Endpoint string
	P256dh   string
	Auth     string
}

func (p *PG) SaveSubscription(ctx context.Context, s PushSub) error {
	_, err := p.pool.Exec(ctx,
		`INSERT INTO push_subscriptions (endpoint, p256dh, auth) VALUES ($1,$2,$3)
		 ON CONFLICT (endpoint) DO UPDATE SET p256dh=EXCLUDED.p256dh, auth=EXCLUDED.auth`,
		s.Endpoint, s.P256dh, s.Auth)
	return err
}

func (p *PG) DeleteSubscription(ctx context.Context, endpoint string) error {
	_, err := p.pool.Exec(ctx, `DELETE FROM push_subscriptions WHERE endpoint=$1`, endpoint)
	return err
}

func (p *PG) LoadSubscriptions(ctx context.Context) ([]PushSub, error) {
	rows, err := p.pool.Query(ctx, `SELECT endpoint, p256dh, auth FROM push_subscriptions`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PushSub
	for rows.Next() {
		var s PushSub
		if err := rows.Scan(&s.Endpoint, &s.P256dh, &s.Auth); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ── cue tasks (outbound scheduler, phase 2) ─────────────────────────
// The durable cue behind cue.Plan: pending tasks feed Plan; inflight tasks (via
// cue.Counts) feed its capacity accounting. Lifecycle: pending → inflight (on
// release/dispatch) → done|failed.

// EnqueueTask inserts a pending task. It is a no-op (returns false) if the task
// id already exists, or if a live task (pending|inflight) already shares this
// task's non-empty DedupeKey — so a generator can re-propose freely without
// duplicating in-flight work. created is the enqueue timestamp (ns).
func (p *PG) EnqueueTask(ctx context.Context, t cue.Task, created int64) (bool, error) {
	tag, err := p.pool.Exec(ctx,
		`INSERT INTO cue_tasks
		   (id, dedupe_key, prompt, priority, target_source, target_session, target_spawn, target_dir, status, created_at)
		 SELECT $1,$2,$3,$4,$5,$6,$7,$8,'pending',$9
		 WHERE $2 = '' OR NOT EXISTS (
		   SELECT 1 FROM cue_tasks WHERE dedupe_key=$2 AND status IN ('pending','inflight'))
		 ON CONFLICT (id) DO NOTHING`,
		t.ID, t.DedupeKey, t.Prompt, t.Priority,
		t.Target.Source, t.Target.SessionID, t.Target.Spawn, t.Target.SpawnDir, created)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// PendingTasks returns tasks awaiting release, highest-priority-first (older
// first on a tie) — the input to cue.Plan.
func (p *PG) PendingTasks(ctx context.Context) ([]cue.Task, error) {
	return p.queryTasks(ctx, `WHERE status='pending' ORDER BY priority DESC, created_at, id`)
}

// InflightTasks returns tasks released but not yet done/failed — pass through
// cue.Counts for Plan's inflight argument.
func (p *PG) InflightTasks(ctx context.Context) ([]cue.Task, error) {
	return p.queryTasks(ctx, `WHERE status='inflight'`)
}

func (p *PG) queryTasks(ctx context.Context, where string) ([]cue.Task, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT id, dedupe_key, prompt, priority, target_source, target_session, target_spawn, target_dir, created_at
		 FROM cue_tasks `+where)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []cue.Task
	for rows.Next() {
		var t cue.Task
		if err := rows.Scan(&t.ID, &t.DedupeKey, &t.Prompt, &t.Priority,
			&t.Target.Source, &t.Target.SessionID, &t.Target.Spawn, &t.Target.SpawnDir, &t.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// MarkInflight transitions a pending task to inflight (on dispatch), recording
// the session it actually runs in (the spawned id for spawns) so the reconciler
// can track it. Guarded on the current status so a double-release is a no-op.
func (p *PG) MarkInflight(ctx context.Context, id, runSession string, releasedAt int64) (bool, error) {
	tag, err := p.pool.Exec(ctx,
		`UPDATE cue_tasks SET status='inflight', run_session=$1, released_at=$2, seen_busy=false
		 WHERE id=$3 AND status='pending'`,
		runSession, releasedAt, id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// InflightRow is an in-flight task with the bookkeeping the reconciler needs.
type InflightRow struct {
	ID         string
	Source     string // target_source (also the run source)
	RunSession string
	Spawn      bool
	SeenBusy   bool
	ReleasedAt int64
}

// InflightRows returns released-not-done tasks for completion reconciliation.
func (p *PG) InflightRows(ctx context.Context) ([]InflightRow, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT id, target_source, run_session, target_spawn, seen_busy, released_at
		 FROM cue_tasks WHERE status='inflight'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []InflightRow
	for rows.Next() {
		var r InflightRow
		if err := rows.Scan(&r.ID, &r.Source, &r.RunSession, &r.Spawn, &r.SeenBusy, &r.ReleasedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// MarkSeenBusy latches that an in-flight task's session has been observed busy,
// so a later return to a free status can be read as completion.
func (p *PG) MarkSeenBusy(ctx context.Context, id string) error {
	_, err := p.pool.Exec(ctx, `UPDATE cue_tasks SET seen_busy=true WHERE id=$1 AND status='inflight'`, id)
	return err
}

// MarkDone / MarkFailed close out an inflight task.
func (p *PG) MarkDone(ctx context.Context, id string, doneAt int64) (bool, error) {
	return p.setStatus(ctx, id, "done", "inflight", "done_at", doneAt)
}
func (p *PG) MarkFailed(ctx context.Context, id string, doneAt int64) (bool, error) {
	return p.setStatus(ctx, id, "failed", "inflight", "done_at", doneAt)
}

func (p *PG) setStatus(ctx context.Context, id, to, from, tsCol string, ts int64) (bool, error) {
	tag, err := p.pool.Exec(ctx,
		`UPDATE cue_tasks SET status=$1, `+tsCol+`=$2 WHERE id=$3 AND status=$4`,
		to, ts, id, from)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// ── scratchpad (user todos / scheduled tasks) ───────────────────────
//
// The user-facing work-list lives in its own table (scratchpad_tasks), not
// cue_tasks: it has a kind, scheduling (run_at/cron), and notes the scheduler
// doesn't need. A promoted todo is copied into cue_tasks by the release path,
// so the two pipelines share dispatch without sharing rows.

const scratchpadSchema = `
CREATE TABLE IF NOT EXISTS scratchpad_tasks (
	id             text   PRIMARY KEY,
	dedupe_key     text   NOT NULL DEFAULT '',
	prompt         text   NOT NULL,
	kind           text   NOT NULL DEFAULT 'todo', -- todo | cue | command
	priority       int    NOT NULL DEFAULT 0,
	target_source  text   NOT NULL DEFAULT '',
	target_session text   NOT NULL DEFAULT '',
	target_spawn   bool   NOT NULL DEFAULT false,
	target_dir     text   NOT NULL DEFAULT '',
	status         text   NOT NULL DEFAULT 'pending', -- pending | inflight | done | failed | completed
	run_at         bigint NOT NULL DEFAULT 0,
	cron           text   NOT NULL DEFAULT '',
	notes          jsonb  NOT NULL DEFAULT '[]',
	created_at     bigint NOT NULL,
	done_at        bigint NOT NULL DEFAULT 0
);
-- One live task per dedupe identity: dedupe_key='' opts out.
CREATE UNIQUE INDEX IF NOT EXISTS scratchpad_dedupe_live ON scratchpad_tasks (dedupe_key)
	WHERE status IN ('pending','inflight') AND dedupe_key <> '';`

// Add inserts a pending scratchpad task. No-op (nil, nil) when the prompt is
// empty or a live task already shares its non-empty DedupeKey — so the model
// can re-propose freely without duplicating work.
func (p *PG) Add(ctx context.Context, t scratchpad.Task) (*scratchpad.Task, error) {
	t = scratchpad.Normalize(t)
	if t.Prompt == "" {
		return nil, nil
	}
	tag, err := p.pool.Exec(ctx,
		`INSERT INTO scratchpad_tasks
		   (id, dedupe_key, prompt, kind, priority, target_source, target_session, target_spawn, target_dir, status, run_at, cron, created_at)
		 SELECT $1,$2,$3,$4,$5,$6,$7,$8,$9,'pending',$10,$11,$12
		 WHERE $2 = '' OR NOT EXISTS (
		   SELECT 1 FROM scratchpad_tasks WHERE dedupe_key=$2 AND status IN ('pending','inflight'))
		 ON CONFLICT (id) DO NOTHING`,
		t.ID, t.DedupeKey, t.Prompt, string(t.Kind), t.Priority,
		t.Target.Source, t.Target.SessionID, t.Target.Spawn, t.Target.SpawnDir,
		t.RunAt, t.Cron, t.CreatedAt)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, nil // dedupe no-op
	}
	out := t
	return &out, nil
}

// List returns scratchpad tasks matching kinds (empty = all): open first
// (pending, then inflight), newest-first within each band; closed after.
func (p *PG) List(ctx context.Context, kinds ...scratchpad.TaskKind) ([]scratchpad.Task, error) {
	where := ""
	args := []any{}
	if len(kinds) > 0 {
		ph := make([]string, len(kinds))
		for i, k := range kinds {
			ph[i] = fmt.Sprintf("$%d", i+1)
			args = append(args, string(k))
		}
		where = " WHERE kind IN (" + strings.Join(ph, ",") + ")"
	}
	rows, err := p.pool.Query(ctx,
		`SELECT id, dedupe_key, prompt, kind, priority, target_source, target_session,
		        target_spawn, target_dir, status, run_at, cron, created_at
		 FROM scratchpad_tasks`+where+`
		 ORDER BY CASE status WHEN 'pending' THEN 0 WHEN 'inflight' THEN 1 ELSE 2 END,
		          created_at DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []scratchpad.Task
	for rows.Next() {
		var t scratchpad.Task
		var kind string
		var status string
		if err := rows.Scan(&t.ID, &t.DedupeKey, &t.Prompt, &kind, &t.Priority,
			&t.Target.Source, &t.Target.SessionID, &t.Target.Spawn, &t.Target.SpawnDir,
			&status, &t.RunAt, &t.Cron, &t.CreatedAt); err != nil {
			return nil, err
		}
		t.Kind = scratchpad.TaskKind(kind)
		t.Status = scratchpad.Status(status)
		out = append(out, t)
	}
	return out, rows.Err()
}

// Complete closes a task (done or failed). Status-guarded on live states so a
// double-complete is a no-op; returns false for unknown ids too.
func (p *PG) Complete(ctx context.Context, id string, done bool) (bool, error) {
	to := string(scratchpad.StatusCompleted)
	if !done {
		to = string(scratchpad.StatusFailed)
	}
	tag, err := p.pool.Exec(ctx,
		`UPDATE scratchpad_tasks SET status=$1, done_at=$2
		 WHERE id=$3 AND status IN ('pending','inflight')`,
		to, time.Now().UnixNano(), id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// Note appends a free-form note to the task's notes array. False for unknown ids.
func (p *PG) Note(ctx context.Context, id, note string) (bool, error) {
	tag, err := p.pool.Exec(ctx,
		`UPDATE scratchpad_tasks SET notes = notes || to_jsonb($1::text) WHERE id=$2`,
		note, id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// Get returns one task by id (nil, nil when absent).
func (p *PG) Get(ctx context.Context, id string) (*scratchpad.Task, error) {
	row := p.pool.QueryRow(ctx,
		`SELECT id, dedupe_key, prompt, kind, priority, target_source, target_session,
		        target_spawn, target_dir, status, run_at, cron, created_at
		 FROM scratchpad_tasks WHERE id=$1`, id)
	var t scratchpad.Task
	var kind, status string
	if err := row.Scan(&t.ID, &t.DedupeKey, &t.Prompt, &kind, &t.Priority,
		&t.Target.Source, &t.Target.SessionID, &t.Target.Spawn, &t.Target.SpawnDir,
		&status, &t.RunAt, &t.Cron, &t.CreatedAt); err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	t.Kind = scratchpad.TaskKind(kind)
	t.Status = scratchpad.Status(status)
	return &t, nil
}
