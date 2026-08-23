package service

import (
	"context"
	"fmt"
	"time"

	"github.com/iodesystems/yscr/source"
)

// waitShellIdle polls a spawned shell session until it reports idle-at-prompt
// again (the command finished), then returns the tail of its output. The first
// poll is delayed so a fast command doesn't read an empty pane before the
// prompt redraws.
func (s *Server) waitShellIdle(ctx context.Context, ref source.SessionRef) (string, error) {
	src := s.sourceByID(ref.Source)
	if src == nil {
		return "", fmt.Errorf("no source %q", ref.Source)
	}
	time.Sleep(2 * time.Second) // let the command start + the prompt settle
	tick := 3 * time.Second
	for {
		st, err := src.State(ctx, ref.ID)
		if err != nil {
			return "", err
		}
		if st.Status == source.StatusIdle {
			return st.Summary, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(tick):
		}
	}
}
