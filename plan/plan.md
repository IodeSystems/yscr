# yscr — plan

> How this plan works: current state + active work + decisions ONLY.
> Completed → `plan/done.md` (one-line pointer). Deferred → `plan/icebox.md`.
> Status: ◻ todo · ◐ in progress · ✅ done · ⏸ parked · ❓ blocked.

## What this is

`github.com/iodesystems/yscr` — **the conversational mediary for your tmux
panes** ("yes sir"). One conversation covers everything running in the
terminal: read, scroll back through, and operate any TUI in a pane; keep a
scratchpad of tasks/todos/schedules; run and watch commands; produce diagrams
and detailed reports on request; and drive multi-step work toward completion —
without ever leaving the conversation or looking at a screen.

**The concierge** is an [agentkit](../agentkit) session on **corrallm**
(the LLM endpoint; corrallm's downstream model adapters are its business), with
audio via **oidio** (STT/TTS). It drives every pane through the one
`source.Source` plugin contract, with a program **Adapter** per supported
application — `source/source.go` + `plugins/pane/adapter.go`:

| adapter | Source | Spawner | Actor |
|---|---|---|---|
| **claude** | live pane + JSONL transcript tail | new tmux session | answer questions (verified keystroke protocol) |
| **terminal** | scrollback + pipe-pane stream | shell window (run commands) | — |

A new supported program = a new Adapter, no new tmux/source code.

A program keeps its own permissioning; the concierge only reads and types.

## Subplans

| file | scope | status |
|---|---|---|
| `plan/scratchpad.md` | todos ✅, schedules ✅, run & watch ✅, completion summaries ✅ | ✅ |
| `plan/goal-plans.md` | goal → dependency graph ✅, open-questions queue ✅, plan_goal ✅ | ✅ |
| `plan/reports.md` | diagrams + detailed reports on request | ✅ core done |
| `plan/decision-memory.md` | decision log, preference auto-resolve | ✅ core done |
| `plan/audio.md` | medium-aware verbosity ✅, ambient narration ✅ (quiet hours + min-interval); streaming STT live drive | ◐ |
| `plan/ops.md` | systemd unit ✅, durable session registries ✅, optional auth ✅ | ✅ |
| `plan/cutover.md` | (superseded) autowork source removed from yscr; remaining work is on the autowork3 side | ⏸ |

Historical design docs (membrane origin, Android client) → `plan/archive/`.

## Active work

### ✅ Task cueing system — outbound scheduler (concierge → panes)
The mirror of the inbound coalescing dispatch: an outbound scheduler that
manages the flow of work TO panes given they are "rarely truly idle"
(so wait-for-idle isn't a viable gate). **Decisions (user, locked):** task
source = concierge-derived (LLM generator tick proposes from pane `State` +
standing goals) · release policy = deterministic status + capacity gate (no
LLM in the hot path) · autonomy = autonomous (`Post`/`Spawn` on its own,
notify after — no confirm step).

**Shape:** cue store (Postgres; survives restart) · generator tick (LLM, slow
cadence) · release loop hooked into the session watcher (polls every 12s) ·
router (existing session via `Post` vs new via `Spawner`).

- ✅ **Data model + deterministic release gate** (`cue/cue.go`: `Task`,
  `Target`, `Caps`, `Plan`). Pure fn: (tasks, session snapshot, in-flight counts,
  caps) → release/hold. Status gate (releasable defaults to idle/done/
  awaiting_user — capacity, not idleness, is what lets work flow to active
  sessions) + per-session/global/spawn caps + priority ordering. 6 tests.
- ✅ **Cue store + config** (`store/pg.go`: `cue_tasks` table, lifecycle
  pending→inflight→done|failed; partial UNIQUE index on `dedupe_key`;
  status-guarded marks → double-release is a no-op). `config.CueConfig` —
  **safe defaults**: `enabled` off, dry-run on when unset.
- ✅ **Release loop** (`service/cue.go`, in the `watch` tick): `PendingTasks` +
  `InflightTasks` → `cue.Plan` → dispatch → `MarkInflight` → `Notify`. Rails:
  kill-switch, `DryRun` (default on) logs intended dispatches, `MaxPerHour`
  sliding window, caps. 6 tests.
- ✅ **Completion detection** (`cueRunner.reconcile`, before release each tick):
  a task completes once its dispatched session has been SEEN BUSY then returns
  to a free status or leaves the set of live sessions; `completion_ttl_seconds` (default 1800)
  backstop reclaims capacity. `seen_busy` latch + `run_session` columns
  (ALTER … IF NOT EXISTS migrates). awaiting_user = still in-flight. Live mode
  is sustainable (capacity actually frees).
- ✅ **Generator tick** (`service/cuegen.go`): on `gen_interval_seconds` cadence,
  shows the LLM session states + `Cue.Goals`, parses strict-JSON `{tasks:[…]}` (tolerant
  extraction), `EnqueueTask`s each (uuid; DedupeKey supplied or derived).
  Enqueue-only → runs even in release dry-run.

**COMPLETE.** generator → durable cue (dedup) → deterministic gate → autonomous
dispatch (rails) → completion reconcile. Every seam unit-tested.

**Switch on** (ships OFF): `cue.enabled:true` + `cue.goals:[…]`; leave
`dry_run` default → logs `cue[dry-run]:` intended dispatches; when satisfied,
`dry_run:false` + `max_per_hour`. Tune caps/`completion_ttl_seconds` as needed.

- **risks:** autonomous `Post`/`Spawn` acts on LIVE sessions unsupervised — a
  bad generator proposal or re-push loop could spam/derail real work. Dedup +
  idempotency + caps are load-bearing, not optional.
- **optional next** → `plan/goal-plans.md`: surface the cue in the PWA;
  priority decay / deadlines; per-source routing policy.

### ✅ The `source.Source` plugin contract (Slice 0)
`source/source.go`: `SessionRef`, `State` (+ `Status`), `Event` (+ `EventKind`),
the capability split — `Source` (List/State/Observe/Post) + optional `Spawner`
+ optional `Actor` (generic `Act(Action{Name,Args})`) — and the
**`Questionnaire`/`Field`/`Option`/`Answer`** crux (form↔conversation,
schema-validated via `source.Validate`). Two backend shapes validate it:
the tmux-hosted CLIs/panes (claude, terminal).

### ✅ Pluggable pane source — generic tmux source + program adapters
`plugins/claudecode` → `plugins/pane`: a generic Source shell (tmux plumbing +
pid↔tty↔pane join + pane scan/classify) parameterized by a program **Adapter**
(`Adapter{ID, Handles, Discover, State, History, Post, Spawn, Act}` + a `Tmux`
plumbing interface). A new program = a new Adapter, no new tmux/source code.
`NewSet` builds one Source per adapter over a shared driver, so
`SessionRef.Source` stays `claude-code` and concierge routing is unchanged.

- ✅ **claude adapter** (`plugins/pane/claude`) — the first + deepest: reads
  Claude's home-dir index (`~/.claude/sessions/*.json`: pid/sessionId/cwd/
  status) + JSONL transcripts; **exact pid→tty→pane adoption** of the user's
  own panes (`/proc/<pid>/fd/0` → `#{pane_tty}` — disambiguates same-cwd
  sessions, self-heals as panes open/close); Resume = `Post` to dormant session
  → `claude --resume <sid>`; Launch = `Spawn(SpawnSpec{Dir,Prompt})`.
  `yscr panes` subcommand: live join view (SID/PANE/STATUS/NAME/CWD).
- ✅ **terminal adapter** (`plugins/pane/terminal`) — the second, proving the
  seam: stateless (no Discover), adopts live `alt=0` panes via the optional
  `Adopter` seam, history from pane scrollback (`capture-pane -S`), Spawn/Act
  unsupported. Declines alt-screen TUIs (no scrollback, captures input). Gated
  by `claude_code.terminal_panes` (default off — adopts every shell).
- ✅ **pipe-pane streaming** (`Tmux.Pipe` + `Streamer` seam +
  `terminal.Stream`). `Source.Observe` delegates to a `Streamer` (live per-line
  feed via `pipe-pane`, ANSI + C0 stripped) or falls back to the one-shot
  summary. `Tmux.Pipe` tails a pipe-pane temp file with an offset watermark
  (cadence bounds latency, not completeness).
- ✅ **claude Streamer** — tails the JSONL transcript from its current end
  (open+seek synchronously; poll for appended records; project each new turn).
  Claude writes at TURN boundaries → narration updates per-turn.
- ✅ **SSE live-tail** (`service/watch.go`): `POST/DELETE /api/watch/{source}/
  {id}` bridges `Observe` → SSE `tail`/`tail-end`; one tail per session,
  idempotent, stops on unwatch/close/pane-exit. PWA: "▶ Watch output" in the
  detail sheet (500-line cap, autoscroll). Note: interactive shells are noisy
  (echoes, prompt redraws); clean for non-interactive output.
- ✅ **concierge narration** (`service/narrate.go`): same `Observe` stream,
  buffer lines, each `narrateInterval` (8s) feed the delta to the LLM with a
  conversational spoken-narration prompt ("convey magnitude, don't recite") →
  `narration` SSE event. PWA: "🔊 Narrate" toggle (ambient — persists across
  sheet close); renders as a dashed/italic concierge bubble + TTS when
  speak-mode is on.
- **deferred** → `plan/audio.md`: ambient auto-narration of active sessions;
  history depth (tool-call aggregation, incremental History via the watermark);
  `send()` paste-buffer fix.

### ✅ claude questionnaires — detect + answer, end to end
KEY FINDING: a *pending* `AskUserQuestion` is NOT in the jsonl — Claude flushes
the tool_use to the transcript only AFTER the turn completes (write-behind).
So the jsonl can't be the read for a live question. **Read = the structured
hook payload; write = tmux send-keys.**

- ✅ **structured question read via PreToolUse hook (primary)** — a
  `PreToolUse`/`AskUserQuestion` hook runs `yscr hook-question`, dropping the
  FULL structured `tool_input` (questions + options + descriptions + multiSelect
  + real `tool_use_id`) to `~/.yscr/pending/<session_id>.json` the instant the
  question is presented — geometry-independent, zero scraping. Answered-
  detection leans on write-behind: the tool_use_id lands in the transcript only
  AFTER the turn completes → its presence means answered → file cleared. State/
  Act prefer the hook; pane-parse is the fallback when the hook isn't installed.
  `yscr install-hook` merges into `~/.claude/settings.json` (idempotent, backs
  up first). **Activation:** run `yscr install-hook` once; only questions asked
  AFTER install get a payload.
- ✅ **pane-parse fallback** (`parsePaneQuestion(capture-pane)`) — scoped to
  the widget (anchored on the `☐` header line) so numbered lists in the
  SCROLLBACK don't leak in as options; preview box-drawing panels stripped from
  labels; stable `questionID` (fnv hash of question+options) so State/Act agree
  + detect drift. Multi-question tab prompts surfaced READ-ONLY here.
- ✅ **Act — the verified keystroke protocol** (`source.Actor`). Captures the
  pane, re-parses, maps each chosen option label → its on-screen digit, drives
  the selector: single-select digit selects+submits; multiSelect toggles each
  digit, `Right`→Review, `1`→Submit. Guards: not-live / no-question-on-screen /
  id drift. **Multi-question tab prompts** (`← ☐ Q1 ☐ Q2 →`) automated:
  single-select digit selects + auto-advances; multi-select digit toggles then
  **Tab** advances; lands on Review → **"1"** submits. **Post-submit
  verification** (`endsOnReview`/`reviewStillOpen`): after a review-ending
  answer, Act re-captures and errors if "Submit answers"/"Ready to submit" is
  still up — catches any keystroke interception instead of falsely claiming
  success. Unit tested; verified end-to-end live on a real 3-question mixed
  prompt (Env/Region/Notify all submitted).
- ✅ **PWA "Needs you" + tap-to-answer** — `#questions` renders every
  `State.Pending` questionnaire below the session strip: source·title, question,
  options as tappable chips (single = one tap; multiSelect = toggle + Submit) →
  `POST /api/answer` (`handleAnswer`) validates against the live questionnaire
  and calls `source.Actor.Act` directly — **NO LLM** — then broadcasts sessions. A
  question is BOTH discussed (concierge `answer_questionnaire` tool, with the
  fix-loop: bad/missing answers return an instruction so the model re-asks) and
  shown (card).

### ✅ P2 — yscr service + PWA
- ✅ **concierge on agentkit** (`concierge/`) — an `agent.Session` with a
  source-aware toolset (session_status / pull_detail / read_history / post /
  spawn / answer_questionnaire) dispatching into the contract, on corrallm
  via agentkit, with its own conversation store. `Converse` = inject user msg
  → Turn → reply.
- ✅ **serialized + coalescing per-session dispatch** (`concierge/queue.go`) —
  each session has one worker goroutine; a turn coalesces everything queued at
  its start into ONE merged turn ("append new work, re-evaluate"); mid-turn
  messages go to the NEXT turn (queue & coalesce, not abort). Background ctx +
  `turnTimeout` (5m) so one caller's cancel can't abort a shared turn.
  **Decision (user): server-side, queue-not-abort.**
- ✅ **service routes** (`service/`, `config/`) — `POST /api/converse`,
  `GET /api/fleet` (aggregated `[]source.State`; the API name predates the
  reframing — it lists every session across all adapters), `/api/health`, embedded
  installable PWA. **Web Push**: auto-generated VAPID keypair, subscribe,
  `Server.Notify` fan-out; `sw.js` background push → showNotification. Push
  needs a secure context (HTTPS or localhost).
- ✅ **SSE + Notify-from-events** — `GET /api/stream` hub + session watcher
  (12s poll, diffs `source.State`): a material transition (new decision awaiting
  you / entered blocked / failed) fires an SSE `notice` (toast + session refresh)
  AND a web-push `Notify`. Baseline primed on start so a restart doesn't
  re-announce in-flight work.
- ✅ **voice (audio proxy + PWA mic/TTS)** — `service/audio.go`: forward-only
  `/api/audio/{capabilities,transcriptions,speech}` relay → oidio (parakeet STT
  / kokoro TTS) via corrallm; key added outbound only, hop-by-hop stripped, 25
  MiB cap, no-redirect SSRF guard. PWA: hold-to-talk mic + 🔊 speak toggle; TTS
  suppressed while the user is speaking (re-checked after the async fetch —
  closes the fetch→play race). `audio.debug_save` tees uploads to
  `~/.yscr/debug-audio/` for VAD diagnosis.
- ✅ **Postgres durable store** (`store/pg.go`, pgx) — isolated `yscr-pg` docker
  (postgres:18, 127.0.0.1:8001, user/db/schema `yscr`). The `agent.Store`
  (concierge conversation + compaction) AND push subscriptions AND cue_tasks.
  `config.Database` DSN; nil → in-memory (`store/mem.go`, volatile). Verified:
  conversation survives a full process restart.

### ◐ P2 remaining / ops
- ◻ **systemd unit** for yscr — the dev auto-reload (`.air.toml` in a detached
  `yscr-air` tmux session; hot-reload verified) dies on reboot. → `plan/ops.md`
- ✅ **narration → push** — ambient narrations fan out via `Notify` (per-session narrate stays SSE-only).
- ◐ **streaming STT** (`service/realtime.go`, `web/pcm-worklet.js`) — oidio's
  realtime WS proxied at `GET /api/audio/realtime`; PCM worklet resamples mic →
  24kHz PCM16 frames → `input_audio_buffer.append`; `.completed` → 700ms
  coalesce → send. TTS→WS→STT verified against the real backend; **remaining:
  drive the browser mic→worklet path live** (only unverified seam). oidio
  endpoint silence is now yaml-configurable (`rule2_silence` = the over-
  segmentation knob). → `plan/audio.md`

## Decisions / conventions
- Module path `github.com/iodesystems/yscr` is FINAL. Public repo.
- Concierge = agentkit consumer; never re-implement the tool loop / compaction.
- yscr has NO external-work plugin: it mediates tmux panes (claude-code,
  terminal). A program keeps its own permissioning; the
  concierge only reads and types.
- **LLM proposes, deterministic layer validates.** The model proposes tasks/
  answers/plans; a thin deterministic gate (validate/dedupe/caps/persist) is the
  hot path. Cue's design set this line — keep it for scratchpad, decision
  memory, and reports.
- Reference: the YSCR footprint inventory — see the autowork3-side coupling map
  in `plan/archive/conversational-membrane.md`.

## How to re-pick-up
1. Read this + `source/source.go` (the contract) + the subplan you're working.
2. Related: [[agentkit]] (the concierge engine); `plan/archive/` for the
   historical membrane/Android-client design docs.
