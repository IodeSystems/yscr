// Package autowork is the source plugin for an autowork3 daemon: it maps
// autowork3's public HTTP API (the P1 fleet seam + thread endpoints) onto the
// yscr source contract. This is the second consumer that validates
// source.Source against a real backend — the concierge drives autowork threads
// through here with no autowork-specific knowledge above the plugin.
//
// It talks to autowork3 over HTTP only (client-token bearer); no shared DB.
// Security gating (send-gate, confused-deputy) stays in autowork3 behind the
// endpoints this calls.
package autowork

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/iodesystems/yscr/source"
)

const sourceID = "autowork"

// The plugin is a full Source, can spawn work, and mediates actions.
var (
	_ source.Source  = (*Client)(nil)
	_ source.Spawner = (*Client)(nil)
	_ source.Actor   = (*Client)(nil)
)

// Client is an autowork3 source plugin.
type Client struct {
	baseURL string // e.g. http://127.0.0.1:8402
	token   string // client bearer token ("" for loopback)
	http    *http.Client
}

// New builds a plugin against an autowork3 daemon.
func New(baseURL, token string, hc *http.Client) *Client {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), token: token, http: hc}
}

func (c *Client) ID() string { return sourceID }

// ── source.Source ───────────────────────────────────────────────────

type fleetThread struct {
	ID            string           `json:"id"`
	Title         string           `json:"title"`
	Status        string           `json:"status"`
	TaskCounts    map[string]int64 `json:"task_counts"`
	OpenDecisions int              `json:"open_decisions"`
}

func (c *Client) fleet(ctx context.Context) ([]fleetThread, error) {
	var out struct {
		Threads []fleetThread `json:"threads"`
	}
	if err := c.getJSON(ctx, "/api/fleet", &out); err != nil {
		return nil, err
	}
	return out.Threads, nil
}

func (c *Client) List(ctx context.Context) ([]source.SessionRef, error) {
	threads, err := c.fleet(ctx)
	if err != nil {
		return nil, err
	}
	refs := make([]source.SessionRef, 0, len(threads))
	for _, t := range threads {
		refs = append(refs, source.SessionRef{Source: sourceID, ID: t.ID, Title: t.Title})
	}
	return refs, nil
}

func (c *Client) State(ctx context.Context, id string) (source.State, error) {
	threads, err := c.fleet(ctx)
	if err != nil {
		return source.State{}, err
	}
	var ft *fleetThread
	for i := range threads {
		if threads[i].ID == id {
			ft = &threads[i]
			break
		}
	}
	if ft == nil {
		return source.State{}, fmt.Errorf("autowork: thread %q not in fleet", id)
	}
	st := source.State{
		Ref:     source.SessionRef{Source: sourceID, ID: ft.ID, Title: ft.Title},
		Status:  deriveStatus(ft),
		Summary: summarize(ft),
	}
	if n := ft.TaskCounts["blocked"]; n > 0 {
		st.Blockers = append(st.Blockers, fmt.Sprintf("%d blocked task(s)", n))
	}
	if ft.OpenDecisions > 0 {
		if qs, err := c.decisions(ctx, id); err == nil {
			st.Pending = qs
		}
	}
	return st, nil
}

func deriveStatus(t *fleetThread) source.Status {
	switch {
	case t.OpenDecisions > 0:
		return source.StatusAwaitingUser
	case t.TaskCounts["blocked"] > 0:
		return source.StatusBlocked
	case t.TaskCounts["failed"] > 0:
		return source.StatusFailed
	case t.TaskCounts["active"] > 0 || t.TaskCounts["feasible"] > 0:
		return source.StatusRunning
	default:
		return source.StatusIdle
	}
}

func summarize(t *fleetThread) string {
	var parts []string
	for _, st := range []string{"active", "feasible", "blocked", "failed"} {
		if n := t.TaskCounts[st]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, st))
		}
	}
	if len(parts) == 0 {
		parts = append(parts, "no open tasks")
	}
	s := strings.Join(parts, ", ")
	if t.OpenDecisions > 0 {
		s += fmt.Sprintf("; %d decision(s) awaiting you", t.OpenDecisions)
	}
	return s
}

// decisions fetches the open decision_requests for one thread as
// questionnaires (fleet-wide endpoint, filtered to id).
func (c *Client) decisions(ctx context.Context, threadID string) ([]source.Questionnaire, error) {
	var out struct {
		Decisions []decisionQuestionnaire `json:"decisions"`
	}
	if err := c.getJSON(ctx, "/api/fleet/decisions", &out); err != nil {
		return nil, err
	}
	var qs []source.Questionnaire
	for _, d := range out.Decisions {
		if d.ThreadID != threadID {
			continue
		}
		qs = append(qs, d.toQuestionnaire())
	}
	return qs, nil
}

type decisionQuestionnaire struct {
	RequestID string          `json:"request_id"`
	ThreadID  string          `json:"thread_id"`
	Title     string          `json:"title"`
	Intro     string          `json:"intro"`
	Fields    []decisionField `json:"fields"`
}
type decisionField struct {
	Key      string           `json:"key"`
	Prompt   string           `json:"prompt"`
	Type     string           `json:"type"`
	Options  []decisionOption `json:"options"`
	Required bool             `json:"required"`
	Help     string           `json:"help"`
}
type decisionOption struct {
	Value  string `json:"value"`
	Label  string `json:"label"`
	Detail string `json:"detail"`
}

func (d decisionQuestionnaire) toQuestionnaire() source.Questionnaire {
	q := source.Questionnaire{ID: d.RequestID, Title: d.Title, Intro: d.Intro}
	for _, f := range d.Fields {
		field := source.Field{
			Key:      f.Key,
			Prompt:   f.Prompt,
			Type:     source.FieldType(f.Type),
			Required: f.Required,
			Help:     f.Help,
		}
		for _, o := range f.Options {
			field.Options = append(field.Options, source.Option{Value: o.Value, Label: o.Label, Detail: o.Detail})
		}
		q.Fields = append(q.Fields, field)
	}
	return q
}

func (c *Client) Post(ctx context.Context, id, message string) error {
	body := map[string]any{"content": message}
	return c.postJSON(ctx, "/api/threads/"+id+"/messages", body, nil)
}

// ── source.Spawner ──────────────────────────────────────────────────

func (c *Client) Spawn(ctx context.Context, spec source.SpawnSpec) (source.SessionRef, error) {
	var created struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := c.postJSON(ctx, "/api/threads", map[string]any{"name": spec.Title}, &created); err != nil {
		return source.SessionRef{}, err
	}
	if spec.Prompt != "" {
		if err := c.Post(ctx, created.ID, spec.Prompt); err != nil {
			return source.SessionRef{}, fmt.Errorf("autowork: spawned thread %s but first message failed: %w", created.ID, err)
		}
	}
	return source.SessionRef{Source: sourceID, ID: created.ID, Title: created.Name}, nil
}

// ── source.Actor ────────────────────────────────────────────────────

// Act mediates a backend action. autowork keeps the actual send-gate /
// confused-deputy gating behind these endpoints — the plugin only translates.
func (c *Client) Act(ctx context.Context, id string, action source.Action) (string, error) {
	switch action.Name {
	case "answer_questionnaire", "apply_decision":
		return c.applyDecision(ctx, id, action.Args)
	case "confirm_send":
		return c.confirmSend(ctx, id, action.Args)
	default:
		return "", fmt.Errorf("autowork: unknown action %q", action.Name)
	}
}

// applyDecision translates a questionnaire Answer (item_id → action verb) into
// autowork3's SubmitDecision payload (item_ids grouped by action) and POSTs it.
func (c *Client) applyDecision(ctx context.Context, threadID string, args map[string]any) (string, error) {
	reqID, _ := args["questionnaire_id"].(string)
	if reqID == "" {
		reqID, _ = args["request_id"].(string)
	}
	if reqID == "" {
		return "", fmt.Errorf("autowork: apply_decision needs questionnaire_id")
	}
	note, _ := args["note"].(string)
	answers, _ := args["answers"].(map[string]any)

	byAction := map[string][]string{}
	for itemID, verb := range answers {
		v, _ := verb.(string)
		if v == "" {
			continue
		}
		byAction[v] = append(byAction[v], itemID)
	}
	if len(byAction) == 0 {
		return "", fmt.Errorf("autowork: no answers to submit")
	}
	verbs := make([]string, 0, len(byAction))
	for v := range byAction {
		verbs = append(verbs, v)
	}
	sort.Strings(verbs) // deterministic decisions[] order
	var decisions []map[string]any
	for _, verb := range verbs {
		ids := byAction[verb]
		sort.Strings(ids)
		d := map[string]any{"item_ids": ids, "action": verb}
		if note != "" {
			d["note"] = note
		}
		decisions = append(decisions, d)
	}

	var out struct {
		Applied   int `json:"applied"`
		Escalated int `json:"escalated"`
		Dismissed int `json:"dismissed"`
	}
	if err := c.postJSON(ctx, "/api/threads/"+threadID+"/decisions/"+reqID+"/submit", map[string]any{"decisions": decisions}, &out); err != nil {
		return "", err
	}
	return fmt.Sprintf("submitted: %d applied, %d escalated, %d dismissed.", out.Applied, out.Escalated, out.Dismissed), nil
}

// confirmSend executes a staged send via autowork3's human-confirm endpoint
// (the send-gate validates the pending). The concierge does the read-back
// first; this only fires on a genuine confirmation.
func (c *Client) confirmSend(ctx context.Context, threadID string, args map[string]any) (string, error) {
	pendingID, _ := args["pending_id"].(string)
	if pendingID == "" {
		return "", fmt.Errorf("autowork: confirm_send needs pending_id")
	}
	var out struct {
		Sent    bool   `json:"sent"`
		Message string `json:"message"`
	}
	if err := c.postJSON(ctx, "/api/threads/"+threadID+"/send-pending/"+pendingID+"/confirm", map[string]any{}, &out); err != nil {
		return "", err
	}
	if out.Sent {
		return "sent.", nil
	}
	return "not sent: " + out.Message, nil
}

// ── HTTP helpers ────────────────────────────────────────────────────

func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	return c.do(req, out)
}

func (c *Client) postJSON(ctx context.Context, path string, body, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(req, out)
}

func (c *Client) do(req *http.Request, out any) error {
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		return fmt.Errorf("autowork: %s %s → %d: %s", req.Method, req.URL.Path, resp.StatusCode, strings.TrimSpace(buf.String()))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// ── source.Source.Observe (SSE) ─────────────────────────────────────

// sseEnvelope is autowork3's SSE data payload (see serveSSEStream).
type sseEnvelope struct {
	Type      string `json:"type"`
	Data      string `json:"data"` // inner JSON string
	Timestamp int64  `json:"timestamp"`
}

// Observe streams a thread's events. It connects to the per-thread SSE feed
// and maps each event to a source.Event; the channel closes when ctx is
// cancelled or the stream ends.
func (c *Client) Observe(ctx context.Context, id string) (<-chan source.Event, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/threads/"+id+"/stream", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		resp.Body.Close()
		return nil, fmt.Errorf("autowork: observe %s → %d", id, resp.StatusCode)
	}

	out := make(chan source.Event, 16)
	ref := source.SessionRef{Source: sourceID, ID: id}
	go func() {
		defer resp.Body.Close()
		defer close(out)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue // comments (":") + blank separators
			}
			var env sseEnvelope
			if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &env) != nil {
				continue
			}
			ev := source.Event{Ref: ref, Kind: mapKind(env.Type), Content: env.Data, At: env.Timestamp}
			select {
			case out <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// mapKind projects autowork3's SSE event type onto the neutral EventKind.
func mapKind(t string) source.EventKind {
	switch t {
	case "fleet_event", "yscr_status", "nudge":
		return source.EventProgress
	default:
		return source.EventProgress
	}
}
