// The summarizer keeps a per-session LLM digest of the fleet, refreshed on a
// throttle. The watch loop calls observe() every tick with the current states;
// for any session whose content changed and whose last summary is older than the
// debounce window, it kicks ONE async LLM summarization (bounded by a small
// semaphore) rather than re-summarizing a busy session on every change. While a
// summary is in flight it emits "activity" so the PWA can show the concierge
// working in the background; when a digest changes it nudges clients to re-pull.
package service

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"

	"github.com/iodesystems/yscr/source"
)

const (
	summaryDebounce      = 30 * time.Second // min gap between LLM summaries of one session
	summaryMaxConcurrent = 2                // cap simultaneous summarizations
	summaryTimeout       = 45 * time.Second
)

const summarySystem = "You are a fleet concierge. Summarize this work session's current state in ONE short sentence, present tense, at most 14 words. No preamble, no quotes, no trailing notes — just the sentence."

type sumEntry struct {
	sig      string    // content signature last summarized (skip if unchanged)
	summary  string    // the LLM digest
	lastAt   time.Time // when we last summarized (throttle anchor)
	inflight bool
}

type summarizer struct {
	runner     agent.LLMRunner
	onActivity func(kind, key, title string) // "summarizing" | "idle"
	onUpdated  func()                         // a digest changed → nudge clients
	sem        chan struct{}

	mu    sync.Mutex
	cache map[string]*sumEntry
}

func newSummarizer(runner agent.LLMRunner, onActivity func(kind, key, title string), onUpdated func()) *summarizer {
	return &summarizer{
		runner:     runner,
		onActivity: onActivity,
		onUpdated:  onUpdated,
		sem:        make(chan struct{}, summaryMaxConcurrent),
		cache:      map[string]*sumEntry{},
	}
}

// summaryFor returns the cached digest for a session key, or "" if none yet.
func (s *summarizer) summaryFor(key string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.cache[key]; e != nil {
		return e.summary
	}
	return ""
}

// observe kicks throttled async summaries for sessions whose content changed.
func (s *summarizer) observe(ctx context.Context, states []source.State) {
	if s.runner == nil {
		return
	}
	for _, st := range states {
		key := sessionKey(st.Ref)
		sig := contentSig(st)
		s.mu.Lock()
		e := s.cache[key]
		if e == nil {
			e = &sumEntry{}
			s.cache[key] = e
		}
		throttled := !e.lastAt.IsZero() && time.Since(e.lastAt) < summaryDebounce
		skip := e.inflight || sig == e.sig || throttled
		if skip {
			s.mu.Unlock()
			continue
		}
		e.inflight = true
		s.mu.Unlock()
		go s.run(ctx, key, st, sig)
	}
}

func (s *summarizer) run(ctx context.Context, key string, st source.State, sig string) {
	s.sem <- struct{}{}
	defer func() { <-s.sem }()

	title := st.Ref.Title
	if title == "" {
		title = st.Ref.ID
	}
	s.onActivity("summarizing", key, title)
	defer s.onActivity("idle", key, title)

	summary, err := s.summarize(ctx, st)

	s.mu.Lock()
	e := s.cache[key]
	e.inflight = false
	e.lastAt = time.Now()
	e.sig = sig
	updated := err == nil && summary != "" && summary != e.summary
	if updated {
		e.summary = summary
	}
	s.mu.Unlock()

	if updated && s.onUpdated != nil {
		s.onUpdated()
	}
}

func (s *summarizer) summarize(ctx context.Context, st source.State) (string, error) {
	raw := strings.TrimSpace(st.Summary)
	if raw == "" {
		return "", nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Status: %s\n", st.Status)
	for _, bl := range st.Blockers {
		fmt.Fprintf(&b, "Blocker: %s\n", bl)
	}
	fmt.Fprintf(&b, "Recent activity:\n%s", raw)

	ctx, cancel := context.WithTimeout(ctx, summaryTimeout)
	defer cancel()
	ch, err := s.runner.ChatStream(ctx, []llm.Message{
		{Role: "system", Content: summarySystem},
		{Role: "user", Content: b.String()},
	}, nil, nil)
	if err != nil {
		return "", err
	}
	var out strings.Builder
	for chunk := range ch {
		if chunk.Error != "" {
			return "", fmt.Errorf("%s", chunk.Error)
		}
		out.WriteString(chunk.Content)
		if chunk.Done {
			break
		}
	}
	return strings.TrimSpace(out.String()), nil
}

func sessionKey(ref source.SessionRef) string { return ref.Source + "/" + ref.ID }

// contentSig changes only when the material content changes — NOT on UpdatedAt,
// which ticks every read — so an unchanged session isn't re-summarized.
func contentSig(st source.State) string {
	return fmt.Sprintf("%s|%d|%s", st.Status, len(st.Pending), st.Summary)
}
