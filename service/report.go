package service

import (
	"context"
	"fmt"

	"github.com/iodesystems/yscr/cue"
	"github.com/iodesystems/yscr/reports"
	"github.com/iodesystems/yscr/store"
)

// cueTaskGraph returns every cue task + a status map for the dependency-graph
// renderer. Nil when there's no durable store (the tool reports it as such).
func (s *Server) cueTaskGraph(ctx context.Context) ([]cue.Task, map[string]string, error) {
	pg, ok := s.pad.(*store.PG)
	if !ok {
		return nil, nil, fmt.Errorf("no durable store")
	}
	pending, err := pg.PendingTasks(ctx)
	if err != nil {
		return nil, nil, err
	}
	inflight, err := pg.InflightTasks(ctx)
	if err != nil {
		return nil, nil, err
	}
	done, live, err := pg.TaskStatuses(ctx)
	if err != nil {
		return nil, nil, err
	}
	all := append(append([]cue.Task{}, pending...), inflight...)
	statuses := map[string]string{}
	for id := range done {
		statuses[id] = "done"
	}
	for id := range live {
		if statuses[id] == "" {
			statuses[id] = "inflight"
		}
	}
	for _, t := range all {
		if _, ok := statuses[t.ID]; !ok {
			statuses[t.ID] = "pending"
		}
	}
	return all, statuses, nil
}

// workListReport returns the scratchpad board (todos/schedules/commands).
func (s *Server) workListReport(ctx context.Context) ([]reports.Task, error) {
	tasks, err := s.pad.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]reports.Task, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, reports.Task{ID: t.ID, Prompt: t.Prompt, Priority: t.Priority, Status: string(t.Status), Kind: string(t.Kind), Cron: t.Cron})
	}
	return out, nil
}

// fleetReport returns the live sessions for the fleet-map renderer.
func (s *Server) fleetReport(ctx context.Context) []reports.Session {
	states := s.fleetStates(ctx)
	out := make([]reports.Session, 0, len(states))
	for _, st := range states {
		title := st.Ref.Title
		if title == "" {
			title = st.Ref.ID
		}
		out = append(out, reports.Session{Source: st.Ref.Source, ID: st.Ref.ID, Title: title, Status: string(st.Status)})
	}
	return out
}

