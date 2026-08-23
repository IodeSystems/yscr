package questions

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"
)

// Mem is an in-memory Store for tests and DSN-less runs.
type Mem struct {
	mu sync.Mutex
	qs map[string]*Question
}

func NewMem() *Mem { return &Mem{qs: map[string]*Question{}} }

func (m *Mem) Add(_ context.Context, q Question) (*Question, error) {
	key := Normalize(q)
	if key == "" {
		return nil, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, other := range m.qs {
		if other.Status == StatusOpen && normalizeText(other.Question) == key {
			return nil, nil // dedupe no-op
		}
	}
	q.Status = StatusOpen
	m.qs[q.ID] = &q
	out := q
	return &out, nil
}

func (m *Mem) List(_ context.Context) ([]Question, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var open, answered []Question
	for _, q := range m.qs {
		cp := *q
		if cp.Status == StatusOpen {
			open = append(open, cp)
		} else {
			answered = append(answered, cp)
		}
	}
	sort.Slice(open, func(i, j int) bool { return open[i].CreatedAt < open[j].CreatedAt })
	sort.Slice(answered, func(i, j int) bool { return answered[i].AnsweredAt > answered[j].AnsweredAt })
	return append(open, answered...), nil
}

func (m *Mem) Answer(_ context.Context, id, answer string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	q, ok := m.qs[id]
	if !ok || q.Status != StatusOpen {
		return false, nil
	}
	q.Status = StatusAnswered
	q.Answer = strings.TrimSpace(answer)
	q.AnsweredAt = time.Now().UnixNano()
	return true, nil
}
