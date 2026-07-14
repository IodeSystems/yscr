# yscr — plan

> How this plan works: current state + active work + decisions ONLY.
> Completed → `plan/done.md` (one-line pointer). Deferred → `plan/icebox.md`.
> Status: ◻ todo · ◐ in progress · ✅ done · ⏸ parked · ❓ blocked.

## What this is

`github.com/iodesystems/yscr` — the **standalone fleet concierge** ("yes sir"),
extracted out of autowork3. A personal, voice-first membrane that observes and
drives work across heterogeneous **session sources**. Standalone (public repo)
so the ToS-sensitive claude-code path lives in a personal concierge, cleanly
separated from autowork3-the-harness.

**The concierge** is an [agentkit](../agentkit) session:
- swappable LLM endpoint — corrallm | OpenRouter | Claude Code CLI (tmux virt)
- audio via **oidio** (STT/TTS) ↔ corrallm
- drives all backends through the one `source.Source` plugin contract

**Sources (plugins)** — `source/source.go`:

| plugin | Source | Spawner | Actor |
|---|---|---|---|
| **autowork** | fleet rollup + event feed (via autowork3 API) | new thread/issue | apply-decision, confirm-send |
| **claude-code** | tmux session token/state stream | new tmux session | — |
| **openai** | conversation token stream (corrallm/OpenRouter) | new conversation | — |

Security stays in the source: autowork keeps its send-gate + confused-deputy
checks; the concierge only mediates (read-back → confirm → call `Act`).

## Consumer / layout

```
yscr/
  source/   ◐ the plugin contract (Source/Spawner/Actor + State/Event/Decision)
  (todo)    concierge (agentkit session + fleet digest + narration)
  (todo)    plugins: autowork/ , claudecode/ , openai/
  (todo)    http (SSE for Android, audio proxy), store (concierge convo + summary)
```

`go.mod` requires agentkit v0.1.0 with `replace => ../agentkit` (local dev
until agentkit is go-gettable).

## Active work

### ◻ Task cueing system — outbound scheduler (concierge → sessions)
The mirror of the inbound coalescing dispatch: an outbound scheduler that manages
the flow of work TO sessions given the fleet is "rarely truly idle" (so
wait-for-idle isn't a viable gate). **Decisions (user, locked):**
- **Task source = concierge-derived.** An LLM *generator* tick proposes candidate
  tasks from fleet `source.State` + standing goals → the cue.
- **Release policy = deterministic status + capacity gate.** No LLM in the hot
  path: release a cued task to a session only when its `Status` permits
  (idle/awaiting_user/done) AND it's under an in-flight cap; else HOLD.
- **Autonomy = autonomous.** It `Post`s/`Spawn`s on its own and notifies after —
  no confirm step.

**Shape:** cue store (Postgres; survives restart) · generator tick (LLM, slow
cadence) · release loop hooked into the existing fleet watcher (already polls
`source.State` every 12s) · router (existing session via `Post` vs new via
`Spawner`).

**Phased build:**
1. ✅ Cue data model + **deterministic release gate** (`cue/cue.go`: `Task`,
   `Target`, `Caps`, `Plan`). Pure fn: (tasks, fleet snapshot, in-flight counts,
   caps) → release/hold decisions, no side effects. Status gate (releasable set
   defaults to idle/done/awaiting_user — capacity, not idleness, is what lets
   work flow to active sessions) + per-session/global/spawn caps + priority
   ordering. 6 tests green (`cue/cue_test.go`).
2. ✅ Cue store + config knobs. `store/pg.go`: `cue_tasks` table (lifecycle
   pending→inflight→done|failed; partial UNIQUE index on `dedupe_key` for live
   rows) + `EnqueueTask` (dedup: skips a live DedupeKey), `PendingTasks`,
   `InflightTasks`, `MarkInflight/Done/Failed` (status-guarded → double-release
   is a no-op). `cue.Counts` builds `Plan`'s inflight map via `Target.Key()`.
   `config.CueConfig` (caps + rails; **safe defaults**: `enabled` off,
   `DryRunEnabled()` true when unset). DB-gated tests green vs yscr-pg
   (`store/cue_test.go`): dedupe/lifecycle + store→Plan round-trip.
3. ✅ Release loop (`service/cue.go`, hooked into `watch` in `stream.go`).
   `cueRunner.release(states)` each 12s tick: `PendingTasks` + `InflightTasks`
   → `cue.Plan` → for each RELEASE, `dispatch` (`Post` existing / `Spawn` new)
   → `MarkInflight` → `Notify`. **Rails wired:** `newCueRunner` returns nil
   unless `Cue.Enabled` (kill-switch) + Postgres; `DryRun` (default on) logs
   intended dispatches without acting; `MaxPerHour` sliding-window cap; caps via
   `cue.Caps`. `cueStore` interface keeps the test DB-free. 6 tests green
   (dry-run/live/held-status/spawn/hourly-cap/enable-gate).
4. ◻ Generator tick: LLM proposes tasks from fleet + `Cue.Goals` → `EnqueueTask`
   (DedupeKey blocks re-proposing live work).

**Completion detection (phase 3.5, before sustained live use):** dispatched
tasks stay `inflight` — nothing marks them `done` yet, so a per-session cap fills
after the first dispatch. Need task→session linkage + `MarkDone` when the session
reaches `StatusDone`. Safe under the default dry-run; **required before live.**

- **next:** phase 4 (generator tick) and/or phase 3.5 (completion detection).
  Cue ships OFF (`Cue.Enabled=false`); to trial: enable with `dry_run:true`,
  watch the `cue[dry-run]:` logs, then set `dry_run:false` + a `max_per_hour`.
- **risks:** autonomous `Post`/`Spawn` acts on LIVE sessions unsupervised — a bad
  generator proposal or a re-push loop could spam/derail real work. Dedup +
  idempotency + caps are load-bearing, not optional.
- **blocking decision (USER):** safety rails for autonomous action — (a) a global
  kill-switch / pause, (b) a dry-run mode that logs intended dispatches without
  acting (recommended for first live run), (c) hard caps (max dispatches/hour,
  max spawns). Confirm these before phase 3 goes live.
- **optional:** priority/deadlines on cued tasks; per-source routing policy.


### ✅ Slice 0 — the `source.Source` plugin contract
- `source/source.go`: `SessionRef`, `State` (+ `Status`), `Event` (+
  `EventKind`), the capability split — `Source` (List/State/Observe/Post) +
  optional `Spawner` + optional `Actor` (generic `Act(Action{Name,Args})`,
  ratified) — and the **`Questionnaire`/`Field`/`Option`/`Answer`** crux
  (form↔conversation, schema-validated). Repo pushed (public).

### ◐ P1 — autowork3 grows the source API (additive, no behavior change)
- ✅ **P1.1 fleet observe** — `GET /api/fleet` (`fleetState` builder shared
  with the `fleet_status` tool). autowork3 `87b8bd3`.
- ✅ **P1.3 decisions-as-questionnaires** — `GET /api/fleet/decisions`
  (`buildDecisionQuestionnaire`: each item → a choice Field). Answer path =
  existing `SubmitDecision`. `bcc5dd9`.
- ✅ **P1.2 event feed** — `GET /api/fleet/stream` (fleet SSE topic +
  `broadcastFleetEvent`, same notable-type gate as `notifyYSCR`, additive
  alongside it). `bee6840`.
- ✅ **P1.4 spawn + act — NO new autowork3 code needed.** Spawn = existing
  `POST /api/threads` + `POST /api/threads/{id}/messages`; Post = the messages
  endpoint; Act = existing `SubmitDecision` + `ConfirmSend`. The whole P1 seam
  is public. Send-gate/confused-deputy gating stays in autowork3.
- auth: client-token bearer (already general).

**P1 COMPLETE** — the autowork-side source seam is fully public (fleet + fleet/
decisions + fleet/stream added; threads/messages/decisions/confirm pre-existing).

### ◐ P2 — yscr service
- ✅ **autowork plugin** (`plugins/autowork`) — HTTP client implementing
  `source.Source` + `source.Spawner` against the P1 endpoints (List/State/
  Observe(SSE)/Post/Spawn); decision_requests → `Questionnaire`. httptest-
  backed tests green. Validates the source contract against a real backend.
- ✅ **concierge on agentkit** (`concierge/`) — an `agent.Session` with a
  source-aware toolset (fleet_status / pull_detail / post / spawn) that
  dispatches into the `source.Source` contract; swappable LLM endpoint (any
  `agent.LLMRunner` = `llm.NewClient` → corrallm/OpenRouter/claude-code-tmux);
  own conversation store (`store.Mem`); `DefaultSystem` persona. `Converse`
  = inject user msg → Turn → reply. Tool-loop test drives a fake source.
- ✅ **autowork `Actor` + `answer_questionnaire` loop — the form↔conversation
  crux, end to end.** `source.Validate` (required + choice/multi option checks
  → fix instruction). Concierge `answer_questionnaire` tool: re-fetch the live
  questionnaire → validate (fix loop: bad/missing answers return an instruction
  so the model re-asks) → hand the validated `Answer` to `source.Actor`.
  autowork `Act`: `answer_questionnaire`/`apply_decision` → group item_ids by
  action verb → `SubmitDecision`; `confirm_send` → `ConfirmSend`. All tested
  (validate / grouped-payload / concierge fix-loop).
- ✅ **openai plugin** (`plugins/openai`) — a source whose sessions ARE
  agentkit conversations this process drives against corrallm/OpenRouter
  (`New(llm.NewClient(base,key,model), store, system)`). Spawn starts a
  conversation, Post advances it, State reports the last reply; a different
  shape from autowork (source-that-is-an-agent) → validates the contract
  against a non-remote backend. Tested (Spawn/Post/State/List).
- ✅ **claude-code plugin** (`plugins/claudecode`) — sessions are Claude Code
  CLI in detached tmux windows, keyed by Claude's own session UUID. Reads
  Claude's home-dir metadata (`~/.claude/sessions/*.json` index: sessionId +
  cwd + status + updatedAt; `~/.claude/projects/<enc-cwd>/<sid>.jsonl`
  transcript). **List** = resumable sessions from the index (newest-first,
  capped). **Resume** = `Post` to a dormant session → `claude --resume <sid>`
  in its cwd. **Launch in a dir** = `Spawn(SpawnSpec{Dir,Prompt})` →
  `claude --session-id <uuid>` under `tmux -c <dir>`. State: live pane if
  running, else transcript tail. Mechanics mirror the `ccoa` bridge.
  `source.SpawnSpec` gained a `Dir` field. Tested (fake ~/.claude + exec seam);
  real index parses (5 sessions). Kill tears down.
  - ✅ **adopt the user's own panes (exact pid→tty→pane join)** — the session
    index `~/.claude/sessions/<pid>.json` is named by the claude PID and carries
    `{pid, sessionId, cwd, status, name}`; the PID's controlling tty
    (`/proc/<pid>/fd/0`) joins to a tmux pane (`#{pane_tty}`). So `paneOf(sid)`
    resolves the exact pane hosting a session — disambiguating multiple claude
    sessions in the SAME cwd (a cwd match can't). `target`: own session →
    adopted pane → resume fallback. Post/State/Observe drive the joined pane;
    recomputed per call (self-heals as panes open/close); Kill only touches
    yscr-owned sessions. **Automatic — every live claude pane is a driveable
    yscr session, no explicit adopt step.** Supersedes the earlier cwd
    heuristic. `Panes(ctx) []PaneInfo` backs the CLI. Linux /proc-specific
    (deploy is Linux). Tested (drive-adopted / no-pane-resume / State-running /
    Panes-join). Still one tmux *session* per launched sid — no fan-out of ours.
  - ✅ **`yscr panes` subcommand** (`cmd/yscr`) — analysis view: prints every
    live Claude session joined to its pane (SID/PANE/STATUS/NAME/CWD). No
    daemon; builds the plugin from config. Verified live: 7 panes, same-cwd
    sessions split by tty.
  - ✅ **claude-code questionnaires (detect + answer, pane-based)** — KEY
    FINDING: a *pending* `AskUserQuestion` is NOT in the jsonl — Claude flushes
    the tool_use to the transcript only AFTER the turn completes (write-behind;
    proven: `"name":"AskUserQuestion"`=0 while the selector is on screen, =1
    after answering, mtime jumps). So the jsonl can't be the read for a live
    question. **Read = the live pane; write = tmux send-keys.**
    - `parsePaneQuestion(capture-pane)` — parses the active selector (footer
      "Enter to select · ↑/↓ to navigate") into a `source.Questionnaire`: one
      Field, options in display order (option i = on-screen digit i+1), drops
      the appended Type-something/Chat/Submit rows, detects `[ ]` → multiSelect.
      Stable `questionID` (fnv hash of question+options) so State/Act agree +
      detect drift. `State` (live-pane branch only — a pending question requires
      a live TUI) sets `Pending` + `Status`→`awaiting_user`.
    - `Act` (`source.Actor`) — captures the pane, re-parses, maps each chosen
      option label → its on-screen digit, and drives the selector: single-select
      the digit selects+submits; multiSelect toggles each digit, `Right`
      →Review, `1` →Submit. Guards: not-live / no-question-on-screen / id drift.
    - Plugs into the delta watcher (awaiting_user rises → SSE notice + push) and
      the concierge digest + `answer_questionnaire` tool for free — so the
      concierge can now discuss AND submit answers to a live Claude CLI.
    - Reverse-engineered the TUI empirically (throwaway probe sessions);
      validated the full loop live: State→awaiting_user with parsed options,
      Act→picked the exact option in a real `claude` pane. Unit tests: pane
      parse (single/multi/no-selector), State awaiting_user, Act
      single/multi/no-pane/no-question. **Open: multi-QUESTION prompts (tab UI)
      not yet automated — keystrokesFor rejects len(Fields)!=1.**

**Three backends now satisfy `source.Source`** — a remote HTTP daemon
(autowork), in-process agentkit conversations (openai), and tmux-hosted CLIs
(claude-code) — the strongest validation the contract holds.
- ✅ **service + PWA** (`config/`, `service/`, `web/`, `cmd/yscr`) — the
  runnable daemon: loads config (~/.yscr/config.json; LLM endpoint, which
  sources, VAPID), builds the concierge + enabled plugins, serves
  `POST /api/converse`, `GET /api/fleet` (aggregated `[]source.State`),
  `/api/health`, and the embedded **installable PWA** (manifest + service
  worker). **Web Push**: auto-generated VAPID keypair, `GET /api/push/vapid`,
  `POST /api/push/subscribe`, `Server.Notify(title,body)` fan-out;
  `sw.js` handles background `push` → `showNotification` + notificationclick
  focus, and caches the app shell (offline). Verified live: health/fleet/
  vapid/shell/sw/manifest all serve. **Push needs a secure context (HTTPS or
  localhost).**
- ✅ **SSE + Notify-from-events** — `GET /api/stream` (SSE hub) + a fleet
  watcher (polls every 12s, diffs `source.State`): a material transition (new
  decision awaiting you / entered blocked / failed) fires an SSE `notice`
  (in-app toast + live fleet refresh) AND a web-push `Notify` to the phone.
  `material()` rules unit-tested; SSE stream verified live. Baseline primed on
  start so a restart doesn't re-announce in-flight work.
- ✅ **voice (audio proxy + PWA mic/TTS)** — `service/audio.go`: forward-only
  `/api/audio/{capabilities,transcriptions,speech}` relay (mirrors autowork3 —
  fixed upstream suffix, key added outbound only, hop-by-hop + inbound-Auth
  stripped, 25 MiB upload cap, no-redirect SSRF guard) → `config.Audio`
  (defaults to the LLM/corrallm endpoint; parakeet STT / kokoro TTS) +
  `/api/audio/config` for the UI. PWA: hold-to-talk mic (getUserMedia +
  MediaRecorder → `/api/audio/transcriptions` → send) + a 🔊 speak toggle that
  plays `/api/audio/speech` on each reply; controls hidden if audio disabled.
  Wiring verified (config + proxy forward); end-to-end blocked only by corrallm
  being DOWN (192.168.1.76:8111 refused) — works once it's up.
- ✅ **Postgres durable store** — isolated `yscr-pg` docker (postgres:18) on
  the hz-allocated port `127.0.0.1:8001`, user/db/schema `yscr`
  (search_path=yscr), persistent volume, `--restart unless-stopped`.
  `store/pg.go` (pgx) is the `agent.Store` (concierge conversation: entries +
  compaction) AND persists push subscriptions. `config.Database` DSN (default
  the yscr-pg); nil → in-memory. **Verified: conversation survives a full
  process restart** (codeword recalled from PG).
- ✅ **sources active** (was flagged off): `~/.yscr/config.json` enables
  openai + claude-code (both verified — the concierge spawned real `claude`
  CLIs in tmux) + autowork (points at 127.0.0.1:8402; live when its daemon is
  up). Voice integrated + round-tripped (TTS→STT via the proxy).
- ✅ **PWA fleet = horizontal card scroller** (`web/`) — was a vertical stack
  (~68px/session); now a horizontal strip of fixed-width (210px) cards
  (dot+title, 2-line-clamped summary), so the fleet occupies ONE card-row of
  vertical space on mobile regardless of session count. Verified live (7 cards,
  one row).
- ✅ **TTS suppressed while the user is speaking** (`web/app.js`) — `speak()`
  is skipped when `userSpeaking()` (hands-free + mid-utterance), and re-checked
  AFTER the async speech fetch (closes the fetch→play race where a reply would
  start over a new utterance before barge-in could cut it). Logic-verified.
- ✅ **transcription snippet capture (debug)** (`service/audio.go`,
  `config.go`) — `audio.debug_save` tees each upload's audio file part to
  `~/.yscr/debug-audio/<ts>.<ext>` (best-effort, saved even if upstream STT
  errors), pruned to newest 300. `GET /api/audio/debug` lists (newest-first),
  `GET /api/audio/debug/{file}` plays/downloads (base-name-validated, no
  traversal). For diagnosing VAD early-cutoff vs recorder clipping. **Enabled
  in config.local.json — now persisting all mic audio.** Verified live
  (save/list/fetch/traversal-guard).
- ◐ **streaming STT — transcription latency (prototype landed, needs live
  browser drive)** (`service/realtime.go`, `web/pcm-worklet.js`, `web/app.js`).
  Root cause of latency: batch flow ate a fixed **2.6s** client silence gate +
  a record-then-POST round trip (`stream=true` on the batch endpoint is fake —
  oidio decodes the whole clip then replays tokens). Fix = oidio's **realtime
  WS** (`GET /v1/realtime`), which endpoints server-side (~0.6s) and streams
  partials → the gate + batch inference both vanish.
  - **WS proxy** `GET /api/audio/realtime` → oidio `/v1/realtime` (gorilla): key
    injected outbound, inbound Authorization dropped, `?model` query forwarded,
    `relayWS` pumps both ways. Same posture as the HTTP audioProxy. Unit-tested
    against a fake upstream (relay + key-inject + drop-inbound + query-forward).
  - **PCM worklet** (`pcm-worklet.js`): taps the mic graph, linear-resamples the
    context rate → **24kHz** (backend rate), PCM16-LE, posts ~85ms frames →
    main thread base64 → `input_audio_buffer.append`.
  - **client** (`app.js`): `startListening` prefers streaming (falls back to the
    MediaRecorder batch path if no AudioWorklet/WS); RMS VAD kept ONLY for
    barge-in + status; `session.update{server_vad}`; `.delta`→live preview,
    `.completed`→**700ms coalesce** then `send()` (oidio's 0.6s endpoint
    over-segments otherwise). New `AudioConfig.RTModel` (default `realtime-stt`)
    surfaced via `/api/audio/config`.
  - **Verified**: full TTS→WS→STT loop against the REAL backend
    (`wss://llm.iodesystems.com/v1/realtime`) — session.created/updated, live
    deltas, exact `.completed`. Backend contract (model/rate/schema/gateway-WS)
    all confirmed. **next**: drive the browser mic→worklet path live (only
    unverified seam; standard Web Audio). **risks**: over-segmentation of long
    monologues — mitigated two ways now: the 700ms client coalesce AND oidio's
    endpoint silence is configurable (see below), so `realtime-stt` can raise
    `rule2_silence` instead of relying on the client patch. **optional**: retire
    the batch path once streaming proven on-device; show partials in the input box.
  - ✅ **oidio endpoint silence configurable** (`../services/oidio`:
    `internal/config/config.go`, `internal/engine/realtime.go`,
    `oidio.example.yaml`) — the streaming recognizer's three sherpa endpoint
    rules are now `ModelSpec` yaml (`rule1_silence`/`rule2_silence`/
    `rule3_min_utterance`), replacing hardcoded 2.4/0.8/20; defaults unchanged.
    Rule2 (end-of-utterance trailing silence) is the over-segmentation knob:
    raise it (~1.2–1.5) so mid-thought pauses don't split a turn. Per-session
    override isn't possible (sherpa endpoint config is recognizer-level), so
    config-file is the right altitude. Config parse tested; engine builds.
    **Deploy note**: the live backend (llm.iodesystems.com) runs its own oidio —
    bump `rule2_silence` there to take effect.
- ✅ **serialized + coalescing per-session dispatch** (`concierge/queue.go`,
  `concierge/concierge.go`) — fixes a real race: `Converse` had NO serialization,
  so rapid voice utterances ran concurrent `Turn`s interleaving writes into the
  shared `agent.Store`. Now each session has one worker goroutine; `Converse`
  enqueues + waits. A turn coalesces everything queued at its start into ONE
  merged turn ("append new work, re-evaluate"); all coalesced callers get that
  turn's reply. Messages arriving mid-turn go to the NEXT turn (queue &
  coalesce, not abort — no half-done source tool actions). Background ctx +
  `turnTimeout` (5m) so one caller's cancel can't abort a shared turn and a
  wedged turn can't jam the session. Tested under `-race`: A alone → B+C merged.
  **Decision (user): server-side, queue-not-abort.** Client keeps its
  append/delta-correct/700ms-wait UX unchanged (server is the authority).
- **Known nuance:** agentkit persists tool RESULTS but not the assistant
  tool-CALL, so replaying a tool-heavy conversation yields orphan `tool`
  messages that can confuse the model (it claimed "no memory" after a
  spawn-heavy turn). Plain chat recall is unaffected. → agentkit hardening
  item (persist the tool_call entry too).
- ◻ **service remaining** — concierge→push hook (narration → Notify); openai/
  claude-code session registries are still in-memory (ephemeral across
  restart — the tmux/convo survive, but the plugin forgets them); systemd unit
  for yscr; optional auth (LAN-only, deferred per Carl).
- ✅ **claude-code questionnaire — PWA visual presentation + tap-to-answer** —
  a "Needs you" section (`#questions`, `web/`) renders every `State.Pending`
  questionnaire below the fleet strip: source·title, the question, and the
  options as tappable chips. Single-choice = one tap answers; multiSelect =
  toggle chips + Submit. Submits to `POST /api/answer` (`service.go`,
  `handleAnswer`) which validates against the live questionnaire (same path as
  the concierge tool) and calls `source.Actor.Act` directly — NO LLM — then
  broadcasts fleet. So a question is now BOTH discussed (concierge) and shown
  (card), per Carl's directive. Verified live against real sessions.
  - **Pane-parse robustness (learned from real questions):** the parser is
    scoped to the widget (anchored on the `☐` header line) so numbered lists in
    the SCROLLBACK (Claude's prose "1. …") no longer leak in as options; preview
    box-drawing panels are stripped from labels; **multi-question tab prompts**
    (`← ☐ Q1 ☐ Q2 →` / "Tab to switch questions") are surfaced READ-ONLY (no
    chips, "answer in the terminal" note) since one card can't drive their tabs.
    Tested (scrollback-ignored / multi-question / preview-stripped). Verified
    live: `homelab-horizon` (multi-question) → read-only; `life` (single) →
    clean chips.
  - **Pane-scrape fragility (superseded as primary):** scraping a TUI is
    brittle (wrapped labels truncate; narrow mobile panes cut options off) —
    kept only as a FALLBACK now.
- ✅ **structured question read via PreToolUse hook (primary)** — the robust
  fix. A `PreToolUse`/`AskUserQuestion` hook runs `yscr hook-question`, which
  drops the FULL structured `tool_input` (questions + options + descriptions +
  multiSelect + real `tool_use_id`) to `~/.yscr/pending/<session_id>.json` the
  instant the question is presented — geometry-independent, zero scraping.
  `hookQuestion(sid)` reads it → clean Questionnaire (id = tool_use_id).
  **Answered-detection leans on write-behind:** the tool_use_id lands in the
  transcript only AFTER the turn completes, so its presence there means answered
  → the plugin clears the stale file. State/Act prefer the hook; pane-parse is
  the fallback when the hook isn't installed. `yscr install-hook` merges the
  hook into `~/.claude/settings.json` (idempotent, backs up first). Verified
  live end-to-end: hook fires → structured State(awaiting_user) with option
  descriptions → Act picks the exact option in a real pane → answered → file
  auto-cleared. Tested (hook pending/answered-clears, State, Act; install-hook
  merge empty/idempotent/preserves-existing).
  - **Activation (Carl):** `yscr install-hook` once (adds the hook globally);
    only questions asked AFTER install get a payload (older ones use the pane
    fallback).
- ◻ **multi-question AskUserQuestion** — `Act` handles single-question prompts;
  a prompt with >1 question uses a tab UI (`← Q1 Q2 ✔ Submit →`) not yet
  automated. `keystrokesFor` rejects `len(Fields)!=1` cleanly.
- ✅ **Deploy (dev proxy via hz):** `hz service create --name ysr --domain
  ysr.iodesystems.com --backend 192.168.1.76:8600 --internal-only
  --internal-dns-ip 192.168.1.160 --health-check /api/health` (mirrors the
  existing internal `ebb` service). Internal DNS resolves ysr → 192.168.1.160
  (HAProxy) → dev-box backend. `proxyUp: true`; PWA + all `/api/*` verified
  serving over the HAProxy TLS path. yscr runs on the dev box on `:8600`.
  - ✅ **dev auto-reload** — `.air.toml` (mirrors autowork3: `go build -o
    ./tmp/main ./cmd/yscr`, `full_bin` = `-config config.local.json -listen
    0.0.0.0:8600`; `include_ext` adds js/css/html/webmanifest/svg since web/ is
    go:embed'd). Runs in a detached tmux session `yscr-air` (attach to inspect).
    Replaced the frozen 10:56 nohup orphan (was serving a since-deleted binary,
    so rebuilds never took). Hot-reload verified (pid rotates on a real write).
  - ◻ still needs a **systemd unit** for reboot-persistence — the `yscr-air`
    tmux session dies on reboot (dev auto-reload ≠ production supervision).
- ✅ **Cert:** hz uses ONE multi-SAN cert per zone (CN=`*.vpn.iodesystems.com`,
  SANs = each enabled subdomain: code/kc/llm/vz/…). NOT a wildcard — each
  subdomain is added to the SAN list individually. `hz service create` wires
  DNS+proxy but does NOT enable SSL for the subdomain; that's a separate
  per-subdomain SSL toggle (Carl enabled it in the hz UI). hz re-issued the
  cert via ACME DNS-01 + reloaded HAProxy. **Valid TLS now serving at
  https://ysr.iodesystems.com.**
- ◻ **narration** — port distill L1 / utterance L2 materiality-gate / durable
  summary from autowork3's `yscr_status.go` for the voice progress channel.

### ◻ P3 — cutover
- delete in-process YSCR from autowork3: `yscr.go`, `yscr_status.go`, the
  `notifyYSCR` hook (api.go:311), `yscr` role + tools + prompt, `sessions.
  yscr_summary` column. Split the shared membrane cores (`concierge.go`,
  `confirm_send.go`) — membrane logic → yscr, send-execution stays.
- repoint the Android client (`android/`, pkg `com.iodesystems.yscr`) at the
  yscr service.
- fix `0002_seed.down.sql` (omits `yscr` role cleanup).

## Decisions / conventions
- Module path `github.com/iodesystems/yscr` is FINAL. Public repo.
- Concierge = agentkit consumer; never re-implement the tool loop / compaction.
- autowork is reached via its **public API only** (client-token auth) — no
  shared DB. Security gating stays in autowork3.
- Reference: the YSCR footprint inventory (what's owned vs. coupled) — see the
  autowork3-side coupling map in the extraction notes.

## How to re-pick-up
1. Read this + `source/source.go` (the contract).
2. If Slice 0 signed off → create+push `IodeSystems/yscr` (public), start P1 in
   autowork3 (`services/autowork3`).
3. Related: [[agentkit]] (the concierge engine), autowork3 `plan/` docs.
