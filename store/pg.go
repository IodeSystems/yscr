package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/yscr/cue"
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
	created_at     bigint NOT NULL,
	released_at    bigint NOT NULL DEFAULT 0,
	done_at        bigint NOT NULL DEFAULT 0
);
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
	if _, err := pool.Exec(ctx, pgSchema); err != nil {
		pool.Close()
		return nil, fmt.Errorf("yscr/store: migrate: %w", err)
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

// MarkInflight transitions a pending task to inflight (on dispatch). Guarded on
// the current status so a double-release is a no-op (returns false).
func (p *PG) MarkInflight(ctx context.Context, id string, releasedAt int64) (bool, error) {
	return p.setStatus(ctx, id, "inflight", "pending", "released_at", releasedAt)
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
