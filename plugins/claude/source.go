package claude

import (
	"context"
	"sort"
	"time"

	"github.com/iodesystems/yscr/source"
)

// Source wraps the claude Adapter with the tmux plumbing, implementing the
// source contract directly. The generic pane shell that used to do this is
// gone — the adapter stands alone now, and a future tmux-native source (the
// Activity package) will replace it wholesale.
type Source struct {
	ad    *Adapter
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

// NewSource builds the claude source over its own tmux driver.
func NewSource(cfg Config) *Source {
	return &Source{
		ad:    New(cfg),
		tmux:  newTmux("", "yscr-cc"),
		now:   func() int64 { return time.Now().UnixNano() },
		limit: 25,
	}
}

// ID is the source id claude sessions present as.
func (s *Source) ID() string { return SourceID }

// List enumerates the adapter's persistent sessions, newest first.
func (s *Source) List(ctx context.Context) ([]source.SessionRef, error) {
	list := s.ad.Discover(ctx)
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

// find resolves a session id to the full Session (with pid/cwd for the join).
func (s *Source) find(ctx context.Context, id string) (Session, bool) {
	for _, ss := range s.ad.Discover(ctx) {
		if ss.ID == id {
			return ss, true
		}
	}
	return Session{}, false
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

// Observe streams the session's transcript (the adapter is a Streamer); if it
// cannot stream, emit the current summary once and close.
func (s *Source) Observe(ctx context.Context, id string) (<-chan source.Event, error) {
	ss, ok := s.find(ctx, id)
	if !ok {
		ss = Session{ID: id, Source: s.ID()}
	}
	ch, err := s.ad.Stream(ctx, ss, s.tmux)
	if err != nil {
		return nil, err
	}
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
