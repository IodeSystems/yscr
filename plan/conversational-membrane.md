> _Ported from autowork3 `plan/` (2026-07-12) — design history for the concierge/membrane before it was extracted into this repo. Paths like `internal/…` and table names are autowork3-internal, as-built when this lived in AW._

# Conversational membrane — a voice/translation layer over detail threads

Status: design, not started. Owner: Carl.

## Purpose

A detail thread (PO / triager / decisions / tasks) is precise and
verbose — nobody wants ten minutes of it read aloud. Put a **membrane**
between the human and the thread: an agent that projects the thread
*down* to short, conversational speech and lifts the human's *inferred
intent* back *up* into real thread actions. Keep all the detail
underneath (auditable, unchanged); the membrane is a lossy, high-level
view with drill-down on request.

It generalises: the same membrane that narrates a thread also **fills
out a `decision_request` by talking to you** — walking the form's items
conversationally and submitting your answers. Email triage is the first
thing you'd voice-drive, but the membrane is generic over any thread +
any decision_request.

## Shape

```
        human (voice)
           │ STT ▲ TTS                         (corrallm: parakeet / kokoro / realtime)
           ▼     │
  ┌──────────────────────────┐
  │  conversational thread     │  ← concierge session
  │  (high-level, lossy)       │     digest-DOWN: notable parent events → short speech
  └──────────┬─────────────────┘     act-UP: inferred intent → real parent actions
             │  pull_detail · post_to_parent · answer_decision
             ▼
  ┌──────────────────────────┐
  │   detail thread            │  ← PO / triager / decision_request / tasks
  │  (precise, auditable)      │     UNCHANGED — full fidelity stays here
  └──────────────────────────┘
```

The detail thread keeps everything. The conversational thread is a
lossy projection + an intent-capture surface. Every consequential thing
the concierge does lands as a real event in the detail thread — so
"keep the detail" is automatic.

## Voice transport — corrallm's OpenAI audio surface (updated 2026-06-27)

corrallm now has a full OpenAI-spec'd audio story on the endpoint
autowork3 already points at (`https://llm.iodesystems.com`). Endpoints:

| capability | endpoint | shape |
|---|---|---|
| batch STT | `/v1/audio/transcriptions`, `/translations` | multipart (`model`+`file`); parakeet — **batch-only** |
| TTS | `/v1/audio/speech` | JSON `{model,input,voice}` → binary audio; kokoro |
| realtime STT | `/v1/realtime` | **two transports** — WS upgrade (PCM16 frames *through* corrallm, metered) OR POST SDP offer (**WebRTC**; corrallm brokers signaling, media P2P) |
| discovery | `/v1/capabilities` | self-describing manifest: endpoints, mode-filtered models, curl/WS examples, diarized shape |
| chat | `/v1/chat/completions` | Qwen3-6-27B-MPT (~220k ctx) |

Extras: **diarization** (speaker-labeled `segments` alongside `.text`)
for multi-party; STT models are **mode-gated** (`batch` / `realtime`) —
parakeet is batch-only, realtime needs a realtime-capable STT model. The
client discovers what's callable via `/v1/capabilities`, never hardcodes.

### The decisive consequence: autowork3 doesn't PROCESS audio

The concierge is a plain text-in/text-out agent — no STT/TTS logic in
the Go server. The audio loop runs at the **browser ↔ corrallm**: mic →
STT → transcript → text to the concierge; concierge reply → TTS →
speaker.

**One caveat the auth model forces (verified 2026-06-29):** the UI talks
only to autowork3 same-origin (`/graphql`, `/api`, …) and holds **no
corrallm credential** — `IODE_LLM_API_KEY` is server-side. So the
browser cannot call corrallm directly. autowork3 therefore needs a
**thin audio PROXY** (forward-only handlers that add the key) so the
credential never reaches the browser. autowork3 *forwards* audio bytes;
it never *processes* them. For realtime, only the SDP **signaling** is
proxied — the media flows P2P browser↔corrallm, so no audio transits
the Go server even then.

- The concierge LLM runs on **Qwen3-6-27B-MPT** (220k ctx — ample for
  summarising a detail thread). One chat model served → no concierge
  model-tier choice today.

## Reuse map — ~70% already exists

- **Child-thread binding + cross-thread messaging.**
  `threads.parent_thread_id` / `parent_session_id`; `EventChildSummoned`
  / `EventChildReported` / `EventParentResponse`. `ApproveThreadProposal`
  already spawns a child thread bound to a parent. The conversational
  thread is exactly this — a child/lens thread of the detail thread.
- **The questionnaire-fill case is structurally solved.** The
  `decision_request` primitive (`choice` / `multi_choice` / `write_in` /
  `batch` + `spec`) *is* the form. The concierge is just **another
  frontend to it**, alongside `DecisionPanel.tsx` — it renders the
  `spec` as dialogue and calls `SubmitDecision`. No new form model.
- **Progressive disclosure has a precedent.** The harness Shaper's LOD
  truncation is the same "short by default, expand on demand" idea, one
  layer up at the dialogue level.

## What's new

1. A **`concierge` role + prompt** (like `triager`): keep it short and
   conversational; summarise the detail thread high-level; infer intent;
   **confirm before anything consequential**; expand to detail only when
   asked.
2. **Three cross-thread tools** for the concierge:
   - `pull_detail(topic?)` — read parent events, summarise (drill-down).
   - `post_to_parent(message)` — inject a `user_message` into the detail
     thread (inferred from the user) → the PO/triager picks it up.
   - `answer_decision(request_id, decisions)` — drive `SubmitDecision`
     on a parent `decision_request` (fill the form via conversation).
3. **Digest-down trigger** (hybrid push-notable / pull-detail): the
   detail thread relays *notable* events (new `decision_request`, a
   `result`) down to the concierge inbox; the concierge `pull_detail`s
   the rest on demand. (Cross-thread delivery is explicit, like the
   existing parent/child events — not raw event subscription.)
4. **Voice loop — UI + corrallm, with a thin autowork3 audio PROXY.**
   The browser drives mic → STT → text → concierge, and concierge text →
   TTS → speaker. autowork3 forwards the corrallm audio calls (to keep
   the key server-side — the browser has no credential) but never
   processes audio. Its only real "voice" logic is the `voice_confirmed`
   provenance on a spoken apply (below).

## The safety landmine — voice-issued actions

Voice + inference is a **lossy path to consequential operations**: STT
mis-transcription, then an LLM inferring intent. This directly threatens
the email-triage `send_never_auto` invariant (Phase 3/4): the concierge
calling `answer_decision` → `SubmitDecision` on a gated `send_reply`
would send mail off a transcription + an inference.

**Locked:**
- **Irreversible actions (send / delete) require an explicit spoken
  read-back confirmation** — the concierge reads the drafted reply / the
  target back and waits for an unambiguous "confirm" before submitting.
  The read-back + confirmation are logged verbatim in the detail thread.
- A bare inferred "send it" **must not reach the executor**. The
  existing executor guard (`send_reply` requires `human=true`) stays the
  backstop; the open question is only what provenance a voice-confirmed
  apply carries.

**Open:** does a voice-confirmed apply count as `human=true`, or get a
distinct `voice_confirmed` provenance (so the audit distinguishes a
typed apply from a spoken one, and policy can treat them differently)?
Recommend a distinct provenance — cheap, and it keeps the lossy path
auditable + independently gateable.

## Membrane vs replacement (locked)

The concierge **wraps** the existing PO/triager — it relays intent up
and narrates results down; the detail thread's machinery is untouched.
NOT a replacement. This preserves every existing guarantee and keeps the
detail thread the single source of truth.

## Open items

- **Lifecycle**: spawn the concierge on-demand (`aw thread <id>
  converse` / a UI "talk" button), or a per-thread voice mode? Lean
  on-demand (mirrors the child-thread spawn pattern).
- **Turn-based vs realtime** for v1: turn-based (STT clip → concierge →
  TTS) is far less plumbing and proves the membrane; `/v1/realtime` WS is
  the better UX, deferrable. Recommend turn-based first.
- **Voice provenance** for SubmitDecision (above) — `human` vs
  `voice_confirmed`.
- **Notable-event policy**: which detail-thread events wake the
  concierge to speak (new decision_request + result are obvious; how
  much else?).
- **Barge-in / interruption** handling for live mode (parking with
  realtime).

## Fleet-level concierge — YSCR (extension — design 2026-07-02)

**Name:** the fleet concierge is **YSCR** ("Your Sentient Concierge
Report-o-AI" — Carl's coinage; expansion approx). Role id `yscr`
throughout code/seed/tools; user-facing persona = YSCR. The per-thread
membrane stays `concierge`; YSCR is the fleet-wide sibling.

The membrane above is **1 concierge child ↔ 1 parent detail thread**.
Carl wants a concierge that **sits on top of ALL threads** and converses
about the state of every active task — a switchboard, not a per-thread
lens. That switchboard is **YSCR**. Grounding (2026-07-02, two explore
passes):

- The per-thread concierge is **shipped**: role + prompt + tools
  (`pull_detail` / `post_to_parent` / `answer_decision` / `confirm_send`),
  `ConverseThread` RPC, `spawnConcierge`, `notifyConcierges`,
  `concierge_test.go`. Its tools **derive a single parent** via
  `conciergeParentID` (`concierge.go:43`) and **ignore any supplied
  `thread_id`** (confused-deputy guard, `TestConciergeTools_OnlyTouchParent`).
  `conciergeParentID` **hard-fails on a no-parent thread** — so the fleet
  case is structurally rejected by every existing concierge tool.
- A session belongs to **exactly one thread** (`sessions.thread_id NOT
  NULL`). Subscriptions are **event-type filters, same-thread only** —
  no thread-id dimension. Cross-thread reach today is push-only
  copy-down: `notifyConcierges` writes a fresh event into the concierge's
  OWN thread via `publishEventTo(te, []Subscriber{...})`.
- Cross-thread reads that already exist: `ListThreads` (all),
  `ListAllActiveSessions`, `ListTasksByStatus`, `ListSupervisoryTasks`.
  **No** aggregate join, **no** "open decision_requests across threads"
  (decisions are folded client-side per thread). Precedent for a
  cross-thread scan: `ListUndeliveredAlerts` (`queries/thread_events.sql:19`).

**The fleet concierge is a NEW role, not the per-thread one with N
parents.** The per-thread design's safety rests on deriving one parent;
a fleet concierge legitimately addresses many threads, so its tools take
an **explicit, per-call-validated `thread_id`**. Confused-deputy is still
closed because each action re-validates scope (`loadDecisionRequest`
already rejects a request whose `thread_id` ≠ the arg); the real
invariant — **send-never-auto + staged read-back `confirm_send`** — lives
in `applyDecisionItems` and is thread-agnostic, so it carries over
unchanged.

### Shape

```
                yscr (own ROOT thread, parent_thread_id NULL)
                  │  fleet_status · pull_thread_detail(tid)
                  │  post_to_thread(tid,msg) · answer_decision(tid,req,…) · confirm_send
     ┌────────────┼───────────────┬───────────────┐
     ▼            ▼               ▼               ▼
  thread A     thread B        thread C   …   (all detail threads, UNCHANGED)
```

### New pieces

1. **`yscr` role + prompt** (seed like `concierge`). Prompt:
   narrate fleet state high-level; name threads by id+name; drill down
   only on ask; confirm before anything consequential; sends always
   staged.
2. **Fleet read model** — a `fleet_status` tool backed by:
   - `ListThreads` filtered to non-terminal (`status NOT IN
     ('closed','failed')`).
   - a new grouped query: task counts by status per thread (one
     `GROUP BY thread_id, status` over `tasks`).
   - a new **open-decision-request scan** across threads, mirroring
     `ListUndeliveredAlerts` (fold `EventDecisionRequest` minus
     `decision_*` at the query layer instead of per-thread in Go).
   - blocked-task rollup (`status='blocked'`) so the human hears "3
     threads waiting on you."
3. **Thread-scoped action tools** (fleet-role-gated, like the per-thread
   guards at `tools.go:1237`):
   - `pull_thread_detail(thread_id, topic?)` — RO read of any thread's
     events; validate thread exists.
   - `post_to_thread(thread_id, message)` — inject `EventUserMessage`
     into a named thread (validate non-terminal). Low-risk intent up.
   - `answer_decision(thread_id, request_id, decisions)` — reuse
     `applyDecisionItems` with `provAssisted`; send items **stage**, never
     execute (`errSendNeverAuto`), exactly as per-thread.
   - `confirm_send` — reuse verbatim.
4. **Digest-down across threads** — a `notifyYSCR` variant of
   `notifyConcierges`: on a **notable** event in ANY thread, copy a notice
   into the fleet concierge's own thread inbox (`publishEventTo`, explicit
   subscriber = the active `yscr` session). Same copy-down
   mechanism; the only new thing is the subscriber lookup (active sessions
   where `role='yscr'`). Notable set (v1): new
   `decision_request`, `result`, `task_failed`, `task_escalated`,
   `child_escalation`.
5. **Lifecycle** — `ConverseYSCR` RPC + `aw fleet converse`: spawn a root
   thread (context NULL) + a `yscr` anchor session; **reuse the
   existing active one if present** (singleton for now — no user model yet).

### Reuse / new tally

- Reuse: `applyDecisionItems`, `confirm_send`, `loadDecisionRequest`
  scope-check, `publishEventTo` copy-down, `ListThreads` /
  `ListTasksByStatus`, the tool-spec + role-gate scaffolding.
- New: one role + prompt, one RPC + CLI verb, one grouped task-rollup
  query, one open-decision scan query, `notifyYSCR`, the
  4 thread-scoped tool bodies.

### Blocking decisions (Carl owns)

- **Singleton vs per-user** fleet concierge — defaulting singleton (no
  user model). If multi-user lands, key the session lookup by user.
- **Direct `post_to_thread` vs must-route-through-per-thread-concierge.**
  Recommend direct for a plain `user_message` (cheap, low-risk); anything
  consequential already funnels through the staged-send substrate, so a
  second concierge hop buys nothing.
- **Notable-event breadth** — the v1 set above is a guess; widen once we
  see what actually needs to wake the human.

### Assumptions to catch

- Cross-thread action from one session into another's event stream is
  fine as long as it lands as a real event in the target (audit intact) —
  same posture the per-thread `post_to_parent` already takes.
- The fleet read stays **read-mostly**; the only writes are `user_message`
  injects and staged decisions. No fleet-level task authoring.

### Implementation slice (server, text-only) — drafted 2026-07-02

Mirrors the shipped per-thread concierge at every step. Ordered so each
sub-slice compiles + tests green before the next. `◻` = todo.

**S0 — refactor the concierge cores to take an explicit `threadID`
(no behavior change).** ✅ (2026-07-02)
Extracted `pullThreadEvents` / `postUserMessageToThread` /
`applyDecisionOnThread` (`concierge.go`) + `confirmSendOnThread`
(`confirm_send.go`); the four `run*` handlers are now thin parent-deriving
wrappers. Dropped an unused `requestID` param on `publishParentDecisionEvent`
(request_id already rides in meta). `concierge_test.go` +
`confirm_send_test.go` green unchanged.
The three action bodies in `concierge.go` derive their target via
`conciergeParentID`. Extract the parametric cores so both the per-thread
concierge (threadID = derived parent) and the fleet concierge (threadID =
validated arg) share one apply path:
- `runPullDetail` → `pullThreadEvents(ctx, threadID, topic)`.
- `runAnswerDecision` → `applyDecisionOnThread(ctx, threadID, requestID,
  decisions)` (keeps `loadDecisionRequest(threadID, …)` scope-check,
  `applyDecisionItems(…, provAssisted)`, the `errSendNeverAuto` staging
  branch — all unchanged).
- `runPostToParent` → `postUserMessageToThread(ctx, threadID, message,
  via)`.
- `runConfirmSend` core → `confirmSendOnThread(ctx, threadID, pendingID,
  heard)`.
Existing concierge handlers become thin wrappers (derive parent → call
core). **Test:** existing `concierge_test.go` stays green — the refactor
is the proof.
*next:* extract cores + re-point concierge wrappers. *risk:* the
send-staging path is subtle; keep `applyDecisionItems` untouched, only
move the threadID plumbing.

**S1 — role + toolset plumbing (data ↔ code parity).** ✅ (2026-07-02)
Added `yscr` role + `role-yscr` context (`prompt=yscr`) + `builtin-yscr_tools`
cluster + role→cluster(+thread_publish) map + `context_id` update in
`0002_seed.up.sql`; `clusterYSCRTools` const/registry/expansion in
`builtin_clusters.go`; the 5 YSCR specs + `toolsForRole` case `"yscr"` in
`tools.go` (`chatOptsForRole` default nil = conversational, mirrors
concierge). Extended the cluster-parity contract test to cover `yscr`.
`go build ./...` + server/repo suites green. **Note:** handlers for the 5
specs are S3 — no `yscr` session is spawned until S4, so nothing dispatches
them yet.
- `0002_seed.up.sql` (edit in place, pre-release): add `yscr`
  to `roles`; a `role-yscr` context (`prompt=yscr`);
  a new `builtin-yscr_tools` toolset (cluster
  `yscr_tools`); map the role→cluster (+ `thread_publish` if it
  needs to speak).
- `builtin_clusters.go`: add `clusterYSCRTools` const, to the
  cluster list, and a `case` returning the 5 fleet specs.
  `builtin_clusters_test.go` enforces parity with `toolsForRole`.
- `tools.go`: `toolsForRole` `case "yscr"` → the 5 specs;
  `chatOptsForRole` mirror **concierge** (return `nil` — conversational,
  NOT `tool_choice:required`).
*risk:* the DB cluster path and the Go switch must agree or
`builtin_clusters_test` fails — change both.

**S2 — cross-thread read model + `fleet_status`.** ✅ (2026-07-02)
`CountTasksByThreadStatus` (queries/tasks.sql → repo `CountTasksByThreadStatus`
+ `TaskStatusCount`) and `ListOpenDecisionRequests` (queries/thread_events.sql
→ `events.Store` method, postgres + **memory** impls) added + `sqlc generate`d.
Open-decision scan is **request-level** (resolved once ANY terminal
decision_* references the request id — whole-request resolution;
item-level partial resolution is the flagged follow-up). `fleet_status`
assembly lives in `runFleetStatus` (yscr.go). Tested via
`TestFleetStatus_AggregatesAcrossThreads` (real-pg task counts + memory
decision scan; resolved decision excluded; own thread skipped).
New queries in `queries/tasks.sql` + `queries/thread_events.sql`, then
`sqlc generate` (no DDL → `schema/deployed.sql` untouched):
- `CountTasksByThreadStatus` — `SELECT thread_id, status, count(*) …
  GROUP BY thread_id, status`.
- `ListOpenDecisionRequests` — cross-thread scan mirroring
  `ListUndeliveredAlerts` (`queries/thread_events.sql:19`): `EventDecisionRequest`
  rows with `NOT EXISTS` a `decision_{applied,dismissed,escalated,broken_out}`
  referencing the same `request_id`. **Trickiest query — the request_id is
  in metadata; the resolution events carry it in metadata too.**
- reuse `ListThreads` (filter non-terminal in Go) + `ListTasksByStatus
  ('blocked')`.
- `fleet_status` tool body (new `yscr.go`) assembles a compact
  digest: per non-terminal thread → name, task counts by status, open
  decisions, blocked-on-you count.
*risk:* the open-decision scan is the one place a wrong join gives the
human a false "nothing waiting." Unit-test it against seeded events.

**S3 — fleet action tools (new specs, fleet-gated, thread_id arg).** ✅ (2026-07-02)
`yscr.go`: `runFleetStatus` / `runPullThreadDetail` / `runPostToThread` /
`runFleetAnswerDecision` / `runFleetConfirmSend` + `yscrResolveThread`
validator. Dispatch (tools.go): `answer_decision` / `confirm_send`
role-branch concierge↔yscr; new yscr-gated cases `fleet_status` /
`pull_thread_detail` / `post_to_thread`. Confused-deputy boundary =
scoped loaders (`loadDecisionRequest` / `validateAndConsumePending`),
proven by `TestYSCRAnswerDecision_ForeignRequestFailsClosed`;
`TestPostToThread_LandsAndRejects` covers land + terminal/nonexistent
reject. Build + vet + server/events/repo suites green.
In `tools.go` add specs `specFleetStatus`, `specPullThreadDetail`,
`specPostToThread`, `specFleetAnswerDecision`, `specFleetConfirmSend`
(the last three = concierge specs + a required `thread_id` string), and
dispatch cases gated `if sess.Role != "yscr" { denied }`. Each
body validates the thread (exists; non-terminal for writes) then calls the
S0 core. `yscr.go` holds the bodies.
*risk:* confused-deputy — every write validates the thread_id and every
decision re-checks scope via `loadDecisionRequest`. No unvalidated arg
reaches an executor.

**S4 — spawn + singleton + trigger surface.** ✅ (2026-07-02)
`spawnYSCR` (yscr.go): singleton (reuses an active `yscr` session found via
`AllActiveSessions`) else creates a ROOT thread (parent nil, context NULL)
+ a `yscr` anchor session (`SummonedByID` nil, `Subscriptions "[]"`) + a
summons audit event. `ConverseYSCR` RPC registered (`POST /api/yscr/converse`)
+ proto regenerated. CLI `aw yscr converse` (`cmd/aw/yscr.go` + main.go
case). Tested by `TestSpawnYSCR_CreatesRootAndIsSingleton` (root, anchor
role, reuse). Full build + vet + server suite green.
`yscr.go`:
- `spawnYSCR(ctx)` — mirror `spawnConcierge` but ROOT thread
  (`ParentThreadID=nil`), context NULL, one `yscr` anchor
  session (`SummonedByID=nil`, `Subscriptions="[]"`). Summons audit event.
- Singleton: before spawning, look up an existing active
  `yscr` session (scan `ListAllActiveSessions` for the role, or
  a new `GetActiveYSCR` query); reuse it.
- `ConverseYSCR` RPC (input: none; output `{yscr_thread_id,
  session_id}`) registered in `graphql.go`; `go generate
  ./internal/genproto`.
- CLI `aw yscr converse` (`cmd/aw/yscr.go` or a `nonIDVerbs` entry) —
  mirror `threadConverse`.
Drive it exactly like the concierge: `sendMessage` to `yscr_thread_id`
routes to the anchor (`GetThreadInitialSession`). No scheduler change.

**S5 — digest-down across threads.** ✅ (2026-07-02)
`notifyYSCR` (yscr.go) wired as a choke-point at the end of `publishEvent`
(api.go): a notable cross-thread event copies a tiny `EventNotification`
into the active YSCR session's inbox via `publishEventTo` (targeted).
Fast-path = type-map lookup first (non-notable majority pays nothing);
only notable events do the `AllActiveSessions` scan; skips YSCR's own
thread (+ the notice is non-notable → no re-entrancy). **Notable set is
HIGH-SIGNAL, not the plan's original list:** `decision_request`,
`task_failed`, `task_escalated`, `child_escalation`, `escalation_exhausted`
— generic `result` is EXCLUDED (every session close would wake YSCR for a
full Turn fleet-wide). Widening to include `result` is deferred to the S8
narration layer, which debounces arrival from utterance. Tested by
`TestNotifyYSCR_DigestsNotableAcrossThreads` (notable delivered,
non-notable ignored, self-notify guarded, source_thread_id stamped). Full
suite + vet green.

**Decision taken (was a blocking decision):** notable breadth = the
high-signal set above for S5; `result` waits for S8.
`notifyYSCR(ctx, notable)` — like `notifyConcierges` but
targets the active fleet session(s) regardless of source thread, via
`publishEventTo([]Subscriber{fleet})`, copying a tiny notice into the
fleet thread. **Fast-path: return immediately when no active fleet
session exists** (mirror `notifyConcierges`' empty-children return) so the
hot path pays one cached lookup. Wire a single choke-point call in
`publishEvent` (`api.go:267`) keyed on a notable-type set
(`decision_request`, `result`, `task_failed`, `task_escalated`,
`child_escalation`) — cleaner than threading calls through every publish
site, and the fast-path keeps it cheap.
*blocking decision (Carl):* notable-type breadth (start with the set
above). *risk:* choke-point in `publishEvent` touches every event — the
no-fleet-session fast-path is load-bearing; benchmark it.

**S6 — prompt.** ✅ (2026-07-02, pulled forward — S4's `spawnYSCR` needs a
purpose) `internal/prompts/yscr.md` + `YSCR()` render (`prompts.go`) +
`YSCRPurpose()` (`server/prompts.go`). Mirrors `concierge.md`: terse
fleet narration, name the thread before acting, confirm before
consequential, staged read-back for sends.

**S7 — `yscr_test.go`** ✅ (2026-07-02) 8 tests, all green:
spawn-creates-root + singleton-reuse; `fleet_status` aggregates ≥2 threads
(real-pg counts + memory decision scan, resolved excluded, own thread
skipped); `post_to_thread` lands + rejects terminal/nonexistent;
`answer_decision` applies assisted + send STAGES (executor never called) +
foreign-request fails closed (confused-deputy); dispatch role-gating +
shared-name role-branch (concierge↔yscr); `notifyYSCR` delivers notable /
ignores non-notable / self-notify guard; `ListOpenDecisionRequests`
**Postgres** scan excludes resolved. Full server/events/repo/prompts +
vet green.

**Build order within the slice:** S0 → S1 → S6 → S2 → S3 → S4 → S5, with
S7 growing alongside. S0+S1 compile-green first (dead role, no tools
fire); everything after adds one capability at a time.

### Acting-membrane track COMPLETE (S0–S7, 2026-07-02)

YSCR is spawnable (`aw yscr converse` / `ConverseYSCR`), drivable (typed
messages route to its anchor), acts fleet-wide through validated-thread_id
tools (reusing the concierge's send-safety substrate), and is woken by
high-signal cross-thread events. Remaining is the **S8+ narration /
progress layer** (the "quick-not-realtime" section above) — the UX layer
on top; it does not block anything shipped here. The voice client
(`plan/android-voice-client.md`) fronts this.

## YSCR narration / progress layer (quick-not-realtime — design 2026-07-02)

Context: realtime voice stacks (e.g. the Gemma-on-Cerebras flow) stream
partial tokens and barge-in. We **can't** — our hardware makes inference
slow. We can be *quick*, not realtime. The trick to feeling responsive
anyway: **separate the world-model from the utterance**, refresh the
world-model off the critical path, and tell the client what the pipeline
is doing at every moment. Carl's four-stage model:

```
 (L0) events pile up ──► (L1) distil → CURRENT SUMMARY ──► (L2) generate next
      fleet buffer          (world-model, slot-gated)        conversation item
                                                             + high-level summary
                                                             ("nothing to report" OK)
                                                                     │
                                                             (L3) conversational layer
                                                                  (corrallm STT/TTS + client)
        └──────────────── (X) status/progress channel to client ───────────────┘
```

**L0 — fleet event buffer.** The digest-down inbox (`notifyYSCR`, S5)
accumulates *notable* cross-thread events into YSCR's thread. This is
"events pile up." No LLM.

**L1 — current summary (world-model, NEW state).** A maintained, rolling
distilled summary of fleet state — the same "short by default" idea as the
harness Shaper's compaction, one level up. Regenerated by a **distiller
pass** that runs only when a compute slot is free (provider lane idle),
debounced by a min-interval, and only when the buffer changed materially.
It is CACHED state, not recomputed per turn — this is what "distilled into
the current summary WHEN slot is available" means. Lives on the YSCR
session (a `fleet_summary` blob + a `summary_rev` counter + `last_event_seen`).

**L2 — conversation item (utterance).** When the user speaks OR a slot
frees and the summary advanced, YSCR generates the next thing to say. It
reads the *current summary* + the *last-spoken summary* and emits a
high-level conversational summary of the delta — which may be "nothing
significant to report." This is the YSCR turn that produces text → TTS.
Cheap because it diffs two summaries, not the raw event firehose.

**L3 — conversational layer.** corrallm STT/TTS + the client (web /
Android). Unchanged from the voice-loop design above.

**X — status / progress channel (cross-cutting, NON-LLM).** The layer
that "tells the client what it's doing." A lightweight status the client
renders so slow generation still feels alive: an enum
`{idle, accumulating, distilling, generating, speaking}` + coarse counters
(pending-event count, summary age / `summary_rev`, last-spoken rev).
Emitted by the pipeline stages themselves (not an LLM turn), delivered over
the **existing per-thread SSE** (`/api/threads/{yscr_thread}/stream`) as a
new `yscr_status` event type. This is the piece that replaces realtime
partial-token streaming for our UX.

### Why the split matters

The expensive work (L1 distil) runs OFF the critical path, opportunistically.
The spoken turn (L2) is a cheap summary-diff. The client is never staring
at a dead mic because X streams stage + progress continuously. That's how
"quick, not realtime" reads as responsive.

### Blocking decisions (Carl owns)

- **Summary state location** — a `fleet_summary` blob on the YSCR session
  vs a dedicated table. Recommend session-scoped blob (mirrors the
  data_bag pattern; no schema table).
- **Distiller trigger** — event-arrival + slot-idle + min-interval
  debounce (recommended) vs a fixed cadence tick.
- **Materiality threshold** — how big a summary delta warrants a *spoken*
  L2 turn vs a quiet `yscr_status` update only ("nothing significant").
- **Status transport** — reuse per-thread SSE with a `yscr_status` event
  (recommended) vs a separate lightweight channel.

### Where it slots

This is the **UX layer on top of the acting membrane** — it builds on S4
(spawn) + S5 (digest-down buffer) and does NOT block S4→S7. Sequenced as
the S8+ track now that the acting fleet concierge (S0–S7) is done.

### Narration sub-slices

**S8a — status/progress channel (non-LLM).** ✅ (2026-07-02)
`yscr_status.go`: phases `{idle, accumulating, distilling, generating,
speaking}` + counters (pending, summary_rev, last_spoken_rev), held
in-memory on the Server (`yscrProgress sync.Map`, ephemeral like
`sendConfirmLocks`). Pushed live on the YSCR thread's SSE stream as a
`yscr_status` event (NOT persisted); `GET /api/yscr/{id}/status` serves the
snapshot on connect (+ `aw yscr status <id>`). Wired transitions:
`notifyYSCR` → `yscrAccumulate` (buffer grew); `scheduler.afterTurn` (yscr
role) → `yscrResetAfterTurn` (Turn consumed the buffer → idle).
`yscrSetPhase` is the seam the L1/L2/L3 stages call. Tested by
`TestYSCRStatus_AccumulateAndReset`. Build + vet + server suite green.

**S8b — current summary state on the YSCR session.** ✅ (2026-07-02)
Added nullable `sessions.yscr_summary TEXT` (0001_init.up.sql +
schema/deployed.sql, mirrored across all 9 sessions SELECTs so they keep
returning `db.Session` — the `issue_id` precedent) + `UpdateYSCRSummary`
query → `sqlc generate`. Repo: `Session.YscrSummary *string` +
`SetYSCRSummary`. Server accessors (yscr_status.go): `yscrCurrentSummary`
(read) + `yscrSetSummary` (persist + bump in-memory `summary_rev` + push
status). **Simplification vs the design note:** only the summary TEXT is
durable; `summary_rev`/`last_spoken_rev`/`last_event_seen` stay in the S8a
in-memory cell (re-derived on restart — distiller just re-folds, cheap).
Tested by `TestYSCRSummary_PersistAndRev`. Full server/repo/events + vet
green. Accessors are the seam S8c (write) + S8d (read) consume.

**S8c — distiller (L1 refresh).** ✅ (2026-07-02, deterministic)
`distillYSCR` (yscr_status.go): phase `distilling` → render `runFleetStatus`
as the world-model snapshot → `yscrSetSummary` (persist + rev++) →
`yscrSettle` (pending→0, idle). Trigger = scheduler tick pass (8)
`reconcileYSCRDistill`: events accumulate BETWEEN ticks (notifyYSCR), the
pass folds the backlog once per tick for any active yscr session with
`yscrPending > 0` — so the accumulate phase is real (NOT inline-on-event,
which would defeat it). Naturally debounced: distilling settles pending to
0, so a no-backlog tick is a no-op. `yscrResetAfterTurn` renamed →
`yscrSettle` (shared by turn-end + distill-end). **Deliberately
deterministic:** the summary IS the fleet_status digest; conversational
compression is L2's job (S8d). Swapping in an LLM-compressed summary is a
one-line replacement of the `runFleetStatus` call (+ then the LLM distiller
would gate on provider-lane idle, a real "slot", not just the tick). Tested
by `TestReconcileYSCRDistill` (folds backlog, settles, rev bumps, no-op when
empty). Full suite + vet green.

**S8d — utterance (L2).** ✅ (2026-07-02)
Materiality gate is SERVER-SIDE (not the LLM self-censoring): `distillYSCR`
now only bumps `summary_rev` when the summary TEXT changed; `yscrClaimUtterance`
fires only when `summary_rev > last_spoken_rev` and no nudge is in flight
(atomic check-and-set `nudged`). Trigger = tick pass (9)
`reconcileYSCRUtterance` → `injectYSCRNarrationNudge`: sets phase
`generating` + drops a targeted `[fleet update]` notification (kind
`yscr_narration`, carrying the current summary + "narrate the delta
briefly") into YSCR's inbox. The scheduler's pending-delivery pass then
dispatches a normal YSCR Turn (the utterance); `afterTurn`(yscr) →
`yscrAfterUtterance` advances `last_spoken_rev = summary_rev`, clears
`nudged`+pending, idle. ANY Turn (proactive or user-driven) counts as
"spoke", so a user reply also satisfies the narration. yscr.md gains a
"Proactive updates" section. Tested: `TestDistillYSCR_OnlyBumpsOnChange`
(materiality), `TestReconcileYSCRUtterance` (wake, double-inject guard,
last_spoken advance, no re-trigger). The LLM-produced utterance TEXT is
smoke-test territory (fake runner in units). Full suite + vet green.

**Server-side narration (S8a–S8d) COMPLETE.** The pipeline runs end-to-end
in Go: notable event → accumulate → tick distill (change-gated) → tick
utterance-wake → YSCR Turn speaks the delta → last_spoken advances. Only
S8e (audio) + the LLM-quality passes remain, both outside the Go server.

**S8e — audio (L3).** ✅ (2026-07-02, client-side) Android push-to-talk:
`AudioRecorder` (16kHz mono WAV) → `POST /api/audio/transcriptions` →
editable transcript → send; 🔊 per message → `POST /api/audio/speech` →
MediaPlayer. STT/TTS model+voice are app Settings (defaults parakeet/kokoro,
verify via `/api/audio/capabilities`). autowork3 only forwards via the
shipped audio proxy — no Go-server audio. See `android/`.

### Open decisions (narration)

- **Distiller trigger** — event-growth + slot-idle + min-interval
  (recommended) vs fixed cadence. (S8c)
- **Materiality threshold** — summary delta size that warrants a *spoken*
  L2 turn vs a quiet status-only update. (S8d)
- **When to widen the S5 notable set** to include `result` — safe once
  S8c/S8d decouple arrival from utterance.

## Build order

Steps 1–2 are ALL of autowork3's work (text). Steps 3–4 are UI +
corrallm-config, with no Go-server audio code. The **fleet extension**
slots as step 1.5 (server-side text, the slice above) and is what the
Android client (`plan/android-voice-client.md`) fronts.

1. **Concierge core (text-first)** — the `concierge` role + prompt + the
   three cross-thread tools + the digest-down trigger, spawned as a child
   of a detail thread. Drive it with typed messages; prove the membrane
   (narrate a thread, post intent up, fill a decision_request) before any
   audio. This is the substance.
2. **Voice-action safety** — read-back confirmation for irreversible
   actions + the `voice_confirmed` provenance through SubmitDecision.
   (autowork3-side; needed before any spoken apply can fire.)
3. **Turn-based voice (v1)** — auth: PROXY through autowork3 (browser
   holds no corrallm key). Push-to-talk for v1 (no VAD / no always-on).
   - autowork3 (thin forward-only handlers, resolve corrallm base_url +
     key from the iode provider/secret): `GET /api/audio/capabilities`
     → corrallm `/v1/capabilities`; `POST /api/audio/transcriptions`
     (multipart) → STT → `{text}`; `POST /api/audio/speech`
     `{input,voice}` → TTS bytes. Forward-only; guard against SSRF /
     open-proxy (fixed upstream = the provider base).
   - UI: a `useVoice` hook (`MediaRecorder` push-to-talk → POST clip →
     transcript); a **Talk** affordance on a concierge thread (transcript
     → editable message box → send via the existing path; reply event →
     `/api/audio/speech` → autoplay); a **converse** button (the
     `ConverseThread` RPC exists, no UI yet) to spawn/open the concierge
     child; and the **staged-send read-back**: on a `send_pending`
     event, TTS the system's verbatim body + surface the `pending_id`,
     then spoken "confirm" → `confirm_send` (→ voice_confirmed) or a tap
     → `POST .../send-pending/{id}/confirm` (→ human). Voice meets the
     safety substrate here — no new safety surface.
4. **Realtime voice (v2, defer)** — autowork3 proxies the `/v1/realtime`
   SDP signaling (key added; media P2P); UI `RTCPeerConnection` to
   corrallm, live partial transcripts → streaming turns + barge-in.
   Separable follow-up; turn-based proves the loop first.
