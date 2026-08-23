package scratchpad

import (
	"context"
	"sort"
	"sync"
)

// Mem is an in-memory Store for tests and DSN-less runs. It implements the
// same dedupe + status-guard semantics as *store.PG so behavior doesn't change
// when the durable store is absent.
type Mem struct {
	mu    sync.Mutex
	tasks map[string]*Task // id → task (status carried on the entry)
	status map[string]Status
	notes  map[string][]string
}

func NewMem() *Mem {
	return &Mem{tasks: map[string]*Task{}, status: map[string]Status{}, notes: map[string][]string{}}
}

// live reports whether a status can still be deduped against.
func live(st Status) bool { return st == StatusPending || st == StatusInflight }

func (m *Mem) Add(_ context.Context, t Task) (*Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t = Normalize(t)
	if t.Prompt == "" {
		return nil, nil
	}
	if t.DedupeKey != "" {
		for _, other := range m.tasks {
			if other.DedupeKey == t.DedupeKey && live(m.status[other.ID]) {
				return nil, nil // dedupe no-op
			}
		}
	}
	st := StatusPending
	m.tasks[t.ID] = &t
	m.status[t.ID] = st
	out := t
	return &out, nil
}

func (m *Mem) List(_ context.Context, kinds ...TaskKind) ([]Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	want := map[TaskKind]bool{}
	for _, k := range kinds {
		want[k] = true
	}
	var out []Task
	for t := range m.tasks {
		if len(want) > 0 && !want[m.tasks[t].Kind] {
			continue
		}
		cp := *m.tasks[t]
		cp.Status = m.status[t]
		out = append(out, cp)
	}
	rank := func(t Task) int {
		switch m.status[t.ID] {
		case StatusPending:
			return 0
		case StatusInflight:
			return 1
		default:
			return 2 // closed
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if rank(out[i]) != rank(out[j]) {
			return rank(out[i]) < rank(out[j])
		}
		return out[i].CreatedAt > out[j].CreatedAt // newest first within a band
	})
	return out, nil
}

func (m *Mem) Complete(_ context.Context, id string, done bool) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, ok := m.status[id]
	if !ok || !live(st) {
		return false, nil
	}
	if done {
		m.status[id] = StatusCompleted
	} else {
		m.status[id] = StatusFailed
	}
	return true, nil
}

func (m *Mem) Note(_ context.Context, id, note string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.tasks[id]; !ok {
		return false, nil
	}
	m.notes[id] = append(m.notes[id], note)
	return true, nil
}

func (m *Mem) Get(_ context.Context, id string) (*Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[id]
	if !ok {
		return nil, nil
	}
	cp := *t
	cp.Status = m.status[id]
	return &cp, nil
}

// Statuses exposes the internal status map for tests.
func (m *Mem) Statuses() map[string]Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]Status, len(m.status))
	for k, v := range m.status {
		out[k] = v
	}
	return out
}

// Notes exposes the notes for tests.
func (m *Mem) Notes(id string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.notes[id]
}
