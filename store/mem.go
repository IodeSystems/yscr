// Package store provides agent.Store implementations for the concierge's own
// conversation persistence (yscr keeps its concierge convo in its OWN store —
// autowork threads live in autowork3, but the membrane dialogue is yscr's).
package store

import (
	"context"
	"sync"

	"github.com/iodesystems/agentkit/agent"
)

// Mem is an in-memory agent.Store: one entry log per session. Sufficient for a
// single-process concierge; a durable store (sqlite/postgres) is a later swap.
type Mem struct {
	mu       sync.Mutex
	sessions map[string][]agent.Entry
}

func NewMem() *Mem { return &Mem{sessions: map[string][]agent.Entry{}} }

// ClaimPending: the concierge has no external inbox in v1 (user messages are
// Injected then a Turn runs synchronously), so nothing is ever "pending" mid-
// turn. Returns 0 — the loop still renders every appended entry via Context.
func (m *Mem) ClaimPending(_ context.Context, _ string, _ int64) (int, error) {
	return 0, nil
}

func (m *Mem) Append(_ context.Context, sessionID string, e agent.Entry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[sessionID] = append(m.sessions[sessionID], e)
	return nil
}

func (m *Mem) Context(_ context.Context, sessionID string) ([]agent.Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	src := m.sessions[sessionID]
	out := make([]agent.Entry, len(src))
	copy(out, src)
	return out, nil
}

func (m *Mem) Compact(_ context.Context, sessionID string, c agent.Compaction) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	subsumed := make(map[string]bool, len(c.Subsumes))
	for _, e := range c.Subsumes {
		subsumed[e.ID] = true
	}
	src := m.sessions[sessionID]
	kept := make([]agent.Entry, 0, len(src)+1)
	for _, e := range src {
		if !subsumed[e.ID] {
			kept = append(kept, e)
		}
	}
	m.sessions[sessionID] = append(kept, c.Marker)
	return nil
}
