// Package source defines the plugin contract every YSCR backend implements —
// the seam that lets the concierge observe and drive heterogeneous session
// sources uniformly:
//
//   - autowork    — threads in an autowork3 daemon (via its API)
//   - claude-code — Claude Code CLI sessions in a tmux virtual terminal
//   - openai      — generic OpenAI-spec conversations (corrallm / OpenRouter)
//
// The concierge (an agentkit session with a swappable LLM endpoint + audio
// via oidio) never special-cases a backend: it lists, observes, and acts
// through this interface. "Which backend" is just which plugin is registered.
//
// Capability split: every source can be observed (Source). Sources that can
// start work implement Spawner; sources with backend-specific mediated
// actions (autowork's apply-decision / confirm-send) implement Actor. Keeping
// spawn/act optional keeps a read-only source (e.g. a status-only feed) valid.
package source

import "context"

// SessionRef identifies one session within a source.
type SessionRef struct {
	Source string // plugin id — "autowork" | "claude-code" | "openai"
	ID     string // source-local session id (autowork thread id, tmux name, …)
	Title  string
}

// Status is the coarse lifecycle a concierge digest cares about.
type Status string

const (
	StatusRunning      Status = "running"
	StatusBlocked      Status = "blocked"
	StatusAwaitingUser Status = "awaiting_user" // a Decision is pending
	StatusDone         Status = "done"
	StatusFailed       Status = "failed"
	StatusIdle         Status = "idle"
)

// State is the concierge-facing rollup of one session for the fleet digest.
type State struct {
	Ref       SessionRef
	Status    Status
	Summary   string          // one-line human rollup
	Blockers  []string        // what's stuck, if anything
	Pending   []Questionnaire // structured input awaiting the user
	UpdatedAt int64           // ns
}

// Questionnaire is a structured request for user input a source surfaces — an
// MCP tool's input schema, an autowork decision_request (with an action
// choice + questions), a quiz, a staged-send confirmation (a degenerate
// yes/no). It is THE crux of the concierge: the handler model renders a
// Questionnaire CONVERSATIONALLY (voice/text) and parses free-form answers
// back into a structured Answer, so the user faces a conversation, never a
// form. It projects cleanly to/from JSON Schema, so the assembled Answer is
// schema-validated (agentkit SchemaValidator + fix loop) before submission
// via Actor.Act.
type Questionnaire struct {
	ID     string
	Title  string
	Intro  string // context to speak before asking
	Fields []Field
}

// FieldType is the shape of one answer — mirrors the JSON-Schema kinds so an
// MCP tool's input schema maps straight onto Fields.
type FieldType string

const (
	FieldText   FieldType = "text"
	FieldNumber FieldType = "number"
	FieldBool   FieldType = "bool"
	FieldChoice FieldType = "choice" // exactly one Option
	FieldMulti  FieldType = "multi"  // any subset of Options
)

// Field is one question. Options is set for choice/multi (e.g. a decision's
// apply/dismiss/escalate/break_out becomes a choice Field).
type Field struct {
	Key      string    // the Answer.Values key
	Prompt   string    // the question, as authored
	Type     FieldType
	Options  []Option  // for choice/multi
	Required bool
	Help     string
}

// Option is one allowed value for a choice/multi Field.
type Option struct {
	Value  string
	Label  string
	Detail string
}

// Answer is the structured response the concierge assembles from the
// conversation and submits back via Actor.Act.
type Answer struct {
	QuestionnaireID string
	Values          map[string]any // Field.Key → parsed value
}

// EventKind classifies an observed happening for the narration + live feed.
type EventKind string

const (
	EventMessage  EventKind = "message"  // human/agent utterance
	EventProgress EventKind = "progress" // work advanced
	EventBlocked  EventKind = "blocked"
	EventPrompt   EventKind = "prompt" // a Questionnaire now awaits the user
	EventDone     EventKind = "done"
	EventFailed   EventKind = "failed"
)

// Event is one observed happening in a session — the raw material the
// concierge distills into narration and pushes to the live feed.
type Event struct {
	Ref     SessionRef
	Kind    EventKind
	Content string
	At      int64 // ns
}

// Source is a backend plugin. Every registered backend implements it; the
// concierge lists + observes through this uniform surface.
type Source interface {
	// ID is the plugin id, matching SessionRef.Source.
	ID() string
	// List enumerates the source's live sessions (its slice of the fleet).
	List(ctx context.Context) ([]SessionRef, error)
	// State returns the digest rollup for one session.
	State(ctx context.Context, id string) (State, error)
	// Observe streams events for narration + the live feed until ctx is
	// cancelled or the session ends.
	Observe(ctx context.Context, id string) (<-chan Event, error)
	// Post injects a user message into a session.
	Post(ctx context.Context, id, message string) error
}

// SpawnSpec describes new work to start. Fields are advisory — each source
// maps what it can (autowork: Title→thread name, Prompt→first issue;
// claude-code: Prompt→the initial CLI prompt; openai: Prompt→first message).
type SpawnSpec struct {
	Title  string
	Prompt string
}

// Spawner is the optional capability to start a new session in a source.
type Spawner interface {
	Source
	Spawn(ctx context.Context, spec SpawnSpec) (SessionRef, error)
}

// Action is a backend-specific mediated action the concierge asks a source to
// perform on the user's behalf. Name is the verb ("apply_decision",
// "confirm_send"); Args is the source-interpreted payload. Kept generic so
// the concierge core stays source-agnostic — the security gating lives in the
// source (e.g. autowork keeps its send-gate + confused-deputy checks).
type Action struct {
	Name string
	Args map[string]any
}

// Actor is the optional capability for mediated actions (autowork's
// apply-decision / confirm-send). Returns a human-readable result.
type Actor interface {
	Source
	Act(ctx context.Context, id string, action Action) (string, error)
}
