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
  CLI processes in detached tmux windows, driven via `tmux new-session /
  send-keys -l / capture-pane -p` (Command configurable; exec-seam for tests).
  Spawn starts + sends the prompt, Post types into the pane, State reads the
  last pane lines, List prunes dead sessions, Kill tears down. Tested (fake
  exec) + tmux command forms verified live.

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
- **Known nuance:** agentkit persists tool RESULTS but not the assistant
  tool-CALL, so replaying a tool-heavy conversation yields orphan `tool`
  messages that can confuse the model (it claimed "no memory" after a
  spawn-heavy turn). Plain chat recall is unaffected. → agentkit hardening
  item (persist the tool_call entry too).
- ◻ **service remaining** — concierge→push hook (narration → Notify); openai/
  claude-code session registries are still in-memory (ephemeral across
  restart — the tmux/convo survive, but the plugin forgets them); systemd unit
  for yscr; optional auth (LAN-only, deferred per Carl).
- ✅ **Deploy (dev proxy via hz):** `hz service create --name ysr --domain
  ysr.iodesystems.com --backend 192.168.1.76:8600 --internal-only
  --internal-dns-ip 192.168.1.160 --health-check /api/health` (mirrors the
  existing internal `ebb` service). Internal DNS resolves ysr → 192.168.1.160
  (HAProxy) → dev-box backend. `proxyUp: true`; PWA + all `/api/*` verified
  serving over the HAProxy TLS path. yscr runs on the dev box (`~/.local/bin/
  yscr -listen 0.0.0.0:8600`, currently a nohup bg process — needs a systemd
  unit for persistence).
- ◐ **Cert:** hz uses ONE multi-SAN cert per zone (CN=`*.vpn.iodesystems.com`,
  SANs = each enabled subdomain: code/kc/llm/vz/…). NOT a wildcard — each
  subdomain is added to the SAN list individually. `hz service create` wires
  DNS+proxy but does NOT enable SSL for the subdomain; that's a separate
  per-subdomain SSL toggle (Carl enabled it in the hz UI). hz then re-issues
  the cert via ACME DNS-01 (async, minutes) + reloads HAProxy. As of last
  check still serving the veliode fallback → re-issuance in flight; will flip
  to valid once ACME + HAProxy reload complete.
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
