package decisions

import (
	"fmt"
	"sort"
	"time"
	"sync"
)

// Mem is the in-memory decision log (used when no Postgres DSN is set and by
// tests). Volatile across restart, like store.Mem.
type Mem struct {
	mu  sync.Mutex
	rows []Decision
	seq int
}

func NewMem() *Mem { return &Mem{} }

func (m *Mem) Add(d Decision) (Decision, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if d.ID == "" {
		m.seq++
		d.ID = fmt.Sprintf("d%04d", m.seq)
	}
	if d.CreatedAt.IsZero() {
		d.CreatedAt = timeNow()
	}
	if d.Status == "" {
		d.Status = StatusOpen
	}
	// Supersede any open decision for the same question+field.
	for i := range m.rows {
		if m.rows[i].QuestionKey == d.QuestionKey && m.rows[i].Status == StatusOpen {
			m.rows[i].Status = StatusSuperseded
			m.rows[i].SupersededBy = d.ID
		}
	}
	m.rows = append(m.rows, d)
	return d, nil
}

func (m *Mem) OpenFor(question, field string) (Decision, bool, error) {
	key, _ := KeyFor(question, field)
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.rows {
		if m.rows[i].QuestionKey == key && m.rows[i].Status == StatusOpen {
			return m.rows[i], true, nil
		}
	}
	return Decision{}, false, nil
}

func (m *Mem) List(statuses ...Status) ([]Decision, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	want := map[Status]bool{}
	for _, s := range statuses {
		want[s] = true
	}
	var out []Decision
	for i := range m.rows {
		if len(want) == 0 || want[m.rows[i].Status] {
			out = append(out, m.rows[i])
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

var timeNow = func() time.Time { return time.Now() }
