// Package openai is a source plugin whose sessions are OpenAI-spec
// conversations the plugin drives itself, via agentkit, against corrallm (or
// OpenRouter). Unlike the autowork plugin — which observes work living in a
// remote daemon — here each session IS an agentkit conversation this process
// owns: Spawn starts one, Post advances it, State reports the last reply.
//
// This is the "source that is an agent": it validates source.Source against a
// backend with a completely different shape from autowork, and doubles as a
// smoke test of the concierge's own LLM endpoint (same corrallm base URL).
package openai

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/iodesystems/agentkit/agent"

	"github.com/iodesystems/yscr/source"
)

const sourceID = "openai"

// DefaultSystem is the persona of a spawned conversation. Override via New.
const DefaultSystem = "You are a focused worker. Answer concisely and do exactly what is asked."

var (
	_ source.Source  = (*Plugin)(nil)
	_ source.Spawner = (*Plugin)(nil)
)

// Plugin manages a set of agentkit conversations against one endpoint.
type Plugin struct {
	runner agent.LLMRunner // llm.NewClient(corrallm/openrouter base, key, model)
	store  agent.Store     // per-session conversation persistence
	system string
	now    func() int64

	mu   sync.Mutex
	seq  int
	sess map[string]*meta
}

type meta struct {
	id, title string
	status    source.Status
	summary   string // last reply (truncated)
	updated   int64
}

// New builds the plugin over a runner (the endpoint) and a store.
func New(runner agent.LLMRunner, st agent.Store, system string) *Plugin {
	if system == "" {
		system = DefaultSystem
	}
	return &Plugin{
		runner: runner, store: st, system: system,
		now:  func() int64 { return time.Now().UnixNano() },
		sess: map[string]*meta{},
	}
}

func (p *Plugin) ID() string { return sourceID }

func (p *Plugin) List(_ context.Context) ([]source.SessionRef, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	refs := make([]source.SessionRef, 0, len(p.sess))
	for _, m := range p.sess {
		refs = append(refs, source.SessionRef{Source: sourceID, ID: m.id, Title: m.title})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].ID < refs[j].ID })
	return refs, nil
}

func (p *Plugin) State(_ context.Context, id string) (source.State, error) {
	p.mu.Lock()
	m := p.sess[id]
	p.mu.Unlock()
	if m == nil {
		return source.State{}, fmt.Errorf("openai: no session %q", id)
	}
	return source.State{
		Ref:       source.SessionRef{Source: sourceID, ID: m.id, Title: m.title},
		Status:    m.status,
		Summary:   m.summary,
		UpdatedAt: m.updated,
	}, nil
}

func (p *Plugin) Post(ctx context.Context, id, message string) error {
	p.mu.Lock()
	_, ok := p.sess[id]
	p.mu.Unlock()
	if !ok {
		return fmt.Errorf("openai: no session %q", id)
	}
	_, err := p.turn(ctx, id, message)
	return err
}

func (p *Plugin) Spawn(ctx context.Context, spec source.SpawnSpec) (source.SessionRef, error) {
	p.mu.Lock()
	p.seq++
	id := fmt.Sprintf("s%d", p.seq)
	p.sess[id] = &meta{id: id, title: spec.Title, status: source.StatusRunning, updated: p.now()}
	p.mu.Unlock()

	if spec.Prompt != "" {
		if _, err := p.turn(ctx, id, spec.Prompt); err != nil {
			return source.SessionRef{}, err
		}
	}
	return source.SessionRef{Source: sourceID, ID: id, Title: spec.Title}, nil
}

// turn injects a message and runs one agentkit Turn, recording the reply as
// the session's summary. A plain chat (no tools) — Dispatch stays nil.
func (p *Plugin) turn(ctx context.Context, id, message string) (string, error) {
	sess := &agent.Session{
		SessionID:  id,
		System:     p.system,
		Store:      p.store,
		Runner:     p.runner,
		SpanPrefix: sourceID,
	}
	if err := sess.Inject(ctx, agent.Entry{Kind: agent.KindUser, Content: message}); err != nil {
		return "", err
	}
	res, err := sess.Turn(ctx)
	p.mu.Lock()
	if m := p.sess[id]; m != nil {
		m.status = source.StatusIdle // awaiting the next message
		m.summary = truncate(res.Reply, 200)
		m.updated = p.now()
	}
	p.mu.Unlock()
	return res.Reply, err
}

// Observe emits the session's latest reply once, then closes. (Live token
// streaming — via agent.Session.OnAssistantToken fanned to subscribers — is a
// later enhancement; the contract only requires a well-behaved channel.)
func (p *Plugin) Observe(_ context.Context, id string) (<-chan source.Event, error) {
	p.mu.Lock()
	m := p.sess[id]
	p.mu.Unlock()
	if m == nil {
		return nil, fmt.Errorf("openai: no session %q", id)
	}
	ch := make(chan source.Event, 1)
	if m.summary != "" {
		ch <- source.Event{
			Ref:     source.SessionRef{Source: sourceID, ID: m.id, Title: m.title},
			Kind:    source.EventMessage,
			Content: m.summary,
			At:      m.updated,
		}
	}
	close(ch)
	return ch, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
