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
- ◻ **P1.4 spawn + act** — spawn endpoint (create thread + author issue; today
  `spawnYSCR` direct `repo.CreateThread`/`CreateSession`) + post-message
  endpoint. apply-decision (`SubmitDecision`) + confirm-send (`ConfirmSend`)
  already exist; keep the send-gate/confused-deputy gating IN autowork3.
- auth: reuse client-token bearer (already general).

### ◻ P2 — yscr service
- plugin framework + **autowork plugin** (consumes P1) first; then claude-code
  + openai plugins.
- concierge on agentkit; port the digest (`runFleetStatus`) + narration
  (distill L1 / utterance L2 materiality gate / durable summary) from
  autowork3's `yscr.go`/`yscr_status.go`.
- own store (concierge conversation + narration summary), own SSE for Android,
  audio proxy (oidio↔corrallm). Runs ALONGSIDE in-process YSCR.

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
