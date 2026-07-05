package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iodesystems/agentkit/agent"
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
);`

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
