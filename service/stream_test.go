package service

import (
	"strings"
	"testing"

	"github.com/iodesystems/yscr/source"
)

func state(status source.Status, pending int) source.State {
	st := source.State{Ref: source.SessionRef{Source: "autowork", ID: "t1", Title: "Fix bug"}, Status: status}
	for i := 0; i < pending; i++ {
		st.Pending = append(st.Pending, source.Questionnaire{ID: "q"})
	}
	return st
}

func TestMaterial(t *testing.T) {
	cases := []struct {
		name     string
		old      snap
		existed  bool
		st       source.State
		wantOK   bool
		wantBody string
	}{
		{"first-seen decision", snap{}, false, state(source.StatusAwaitingUser, 1), true, "1 decision"},
		{"decision count rises", snap{source.StatusAwaitingUser, 1}, true, state(source.StatusAwaitingUser, 2), true, "2 decision"},
		{"decision unchanged", snap{source.StatusAwaitingUser, 2}, true, state(source.StatusAwaitingUser, 2), false, ""},
		{"first-seen blocked", snap{}, false, state(source.StatusBlocked, 0), true, "blocked"},
		{"entered blocked", snap{status: source.StatusRunning}, true, state(source.StatusBlocked, 0), true, "blocked"},
		{"still blocked", snap{status: source.StatusBlocked}, true, state(source.StatusBlocked, 0), false, ""},
		{"entered failed", snap{status: source.StatusRunning}, true, state(source.StatusFailed, 0), true, "failed"},
		{"still running", snap{status: source.StatusRunning}, true, state(source.StatusRunning, 0), false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, body, ok := material(c.old, c.existed, c.st)
			if ok != c.wantOK {
				t.Fatalf("ok=%v want %v (body=%q)", ok, c.wantOK, body)
			}
			if ok && !strings.Contains(body, c.wantBody) {
				t.Errorf("body=%q want contains %q", body, c.wantBody)
			}
		})
	}
}
