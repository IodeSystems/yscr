package pane

import (
	"context"
	"sort"
	"time"

	"github.com/iodesystems/yscr/source"
)

// Source is the generic tmux-pane source shell, parameterized by one program
// Adapter. It implements the full source contract (Source/Spawner/Actor/
// Historian) by owning the tmux plumbing and delegating program semantics to the
// adapter. Registering a new program is a new Adapter — no new source/tmux code.
//
// One Source wraps one Adapter and reports the adapter's id as its source id, so
// the concierge's per-source routing (SessionRef.Source) is preserved. Multiple
// adapters = multiple Sources over a shared tmuxDriver (see NewSet).
type Source struct {
	ad    Adapter
	tmux  *tmuxDriver
	now   func() int64
	limit int
}

var (
	_ source.Source    = (*Source)(nil)
	_ source.Spawner   = (*Source)(nil)
	_ source.Actor     = (*Source)(nil)
	_ source.Historian = (*Source)(nil)
)

// Config tunes a Source. Zero value is usable.
type Config struct {
	Tmux   string // tmux binary; "" → "tmux"
	Prefix string // launched-window name prefix; "" → "yscr-cc"
	Limit  int    // max sessions List returns; 0 → 25
}

// New builds a Source over one adapter with its own tmux driver.
func New(ad Adapter, cfg Config) *Source {
	return newWith(ad, newTmux(cfg.Tmux, cfg.Prefix), cfg.Limit)
}

// NewSet builds one Source per adapter over a SHARED tmux driver — the multi-
// program registration. Each Source keeps the adapter's id; the plumbing (exec
// seam, pid↔pane join, launched-window tracking) is common.
func NewSet(cfg Config, adapters ...Adapter) []source.Source {
	t := newTmux(cfg.Tmux, cfg.Prefix)
	out := make([]source.Source, 0, len(adapters))
	for _, ad := range adapters {
		out = append(out, newWith(ad, t, cfg.Limit))
	}
	return out
}

func newWith(ad Adapter, t *tmuxDriver, limit int) *Source {
	if limit <= 0 {
		limit = 25
	}
	return &Source{ad: ad, tmux: t, now: func() int64 { return time.Now().UnixNano() }, limit: limit}
}

func (s *Source) ID() string { return s.ad.ID() }

// sessions assembles the adapter's addressable sessions: its persistent set
// (Discover) unioned with any live panes it adopts (stateless adapters via
// Adopter; a pane already covered by a discovered session's pid is skipped).
func (s *Source) sessions(ctx context.Context) []Session {
	byID := map[string]Session{}
	pids := map[int]bool{}
	for _, ss := range s.ad.Discover(ctx) {
		byID[ss.ID] = ss
		if ss.Pid != 0 {
			pids[ss.Pid] = true
		}
	}
	if adopter, ok := s.ad.(Adopter); ok {
		for _, lp := range s.tmux.scan(ctx) {
			if !s.ad.Handles(lp.Program) || pids[lp.Pid] {
				continue
			}
			if ss, ok := adopter.Adopt(lp); ok {
				byID[ss.ID] = ss
			}
		}
	}
	out := make([]Session, 0, len(byID))
	for _, ss := range byID {
		out = append(out, ss)
	}
	return out
}

// find resolves a session id to the full Session (with pid/cwd for the join).
func (s *Source) find(ctx context.Context, id string) (Session, bool) {
	for _, ss := range s.sessions(ctx) {
		if ss.ID == id {
			return ss, true
		}
	}
	return Session{}, false
}

func (s *Source) List(ctx context.Context) ([]source.SessionRef, error) {
	list := s.sessions(ctx)
	sort.Slice(list, func(i, j int) bool { return list[i].UpdatedAt > list[j].UpdatedAt })
	if len(list) > s.limit {
		list = list[:s.limit]
	}
	refs := make([]source.SessionRef, 0, len(list))
	for _, ss := range list {
		refs = append(refs, source.SessionRef{Source: s.ID(), ID: ss.ID, Title: titleOf(ss), Dir: ss.Cwd})
	}
	return refs, nil
}

func (s *Source) State(ctx context.Context, id string) (source.State, error) {
	ss, ok := s.find(ctx, id)
	if !ok {
		// Unknown to discovery — hand a bare session to the adapter, which may
		// still resolve it (claude: dormant, cwd via its index).
		ss = Session{ID: id, Source: s.ID()}
	}
	return s.ad.State(ctx, ss, s.tmux)
}

func (s *Source) History(ctx context.Context, id string, n int) (string, error) {
	ss, ok := s.find(ctx, id)
	if !ok {
		ss = Session{ID: id, Source: s.ID()}
	}
	return s.ad.History(ctx, ss, n, s.tmux)
}

func (s *Source) Post(ctx context.Context, id, message string) error {
	ss, ok := s.find(ctx, id)
	if !ok {
		ss = Session{ID: id, Source: s.ID()}
	}
	return s.ad.Post(ctx, ss, message, s.tmux)
}

func (s *Source) Spawn(ctx context.Context, spec source.SpawnSpec) (source.SessionRef, error) {
	ss, err := s.ad.Spawn(ctx, spec, s.tmux)
	if err != nil {
		return source.SessionRef{}, err
	}
	return source.SessionRef{Source: s.ID(), ID: ss.ID, Title: titleOf(ss), Dir: ss.Cwd}, nil
}

func (s *Source) Act(ctx context.Context, id string, action source.Action) (string, error) {
	ss, ok := s.find(ctx, id)
	if !ok {
		ss = Session{ID: id, Source: s.ID()}
	}
	return s.ad.Act(ctx, ss, action, s.tmux)
}

// Observe emits the session's current summary once, then closes — matching the
// prior one-shot behavior. A streaming Observe is a later slice.
func (s *Source) Observe(ctx context.Context, id string) (<-chan source.Event, error) {
	ch := make(chan source.Event, 1)
	st, err := s.State(ctx, id)
	if err == nil && st.Summary != "" {
		ch <- source.Event{Ref: st.Ref, Kind: source.EventProgress, Content: st.Summary, At: s.now()}
	}
	close(ch)
	return ch, nil
}

func titleOf(ss Session) string {
	if ss.Name != "" {
		return ss.Name
	}
	return baseName(ss.Cwd)
}

func baseName(dir string) string {
	if dir == "" {
		return "session"
	}
	for i := len(dir) - 1; i >= 0; i-- {
		if dir[i] == '/' {
			return dir[i+1:]
		}
	}
	return dir
}
