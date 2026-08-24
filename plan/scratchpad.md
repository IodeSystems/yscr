# Subplan — scratchpad & command running (horizon work)

> User-facing tasks/todos/schedules + run-and-watch terminal commands.
> The cue system is the outbound scheduler; this adds the INBOUND path from
> conversation and a durable work-list the user can see and steer.

## Purpose
The concierge keeps a scratchpad of horizon work: "add a todo", "remind me at
9", "run X in the background and tell me when it's done". The list is durable,
survives restart, and is visible in the PWA — not just something the model
carries in-context.

## Requirements
1. **Tasks/todos** — concierge tools: `add_task` / `list_tasks` / `done_task`
   (note). Durable store; user can also see them as cards in the PWA.
2. **Schedules/cron** — a task may carry `run_at` or a cron spec; a scheduler
   tick releases it into the cue pipeline (reuses `EnqueueTask` + release gate).
3. **Run & watch commands** — concierge tool: spawn/adopt a terminal pane, run
   a command, tail its output via the existing `Streamer`, and report materiality
   (reuse narrate's LLM-delta prompt). Foreground = wait + summarize; background
   = notify on exit/material event.

## Design constraints
- **LLM proposes, deterministic layer validates** — task entries are validated/
  deduped/persisted by a thin gate (same pattern as `EnqueueTask`).
- Reuse the cue store where shapes match (`cue_tasks` already has lifecycle +
  dedupe); user tasks may need a `kind` column (todo|scheduled|command) rather
  than a new table.
- Run & watch rides the pane/terminal adapter — no new tmux plumbing; Spawn is
  currently unsupported there, so either lift that gate or route through the
  claude adapter's spawn into a shell.

## Status
- ✅ **Slice 1 — todos** (`c7bd92a`): `scratchpad` pkg (Task/Store/Normalize +
  Mem), Postgres store, concierge add_task/list_tasks/done_task tools,
  `GET /api/tasks` + `POST /api/tasks/{id}/done`, PWA work-list section.
- ✅ **Slice 2 — schedules** (`c7bd92a`): cron parser (5-field, minute
  resolution) + scheduler tick in the watch loop: due one-shots promote into
  the cue pipeline; completed cron tasks re-arm at the next occurrence.
- ✅ **Slice 3 — run & watch** (`79dbc86`): terminal adapter `Spawn` (detached
  sh window, command typed in) + concierge `run_command` tool — foreground
  waits for idle-at-prompt (bounded) and reports the output tail; background
  returns the pane id. Live-verified: launched shell windows run commands and
  their scrollback carries the output.
- ✅ **LLM-distilled completion summary** — `run_command` foreground now takes
  an optional summarizer (`WithRun(spawner, wait, summarize...)`); past ~1200
  chars of output the LLM distills it into 1-3 sentences (what happened + the
  facts that matter), raw tail kept underneath; short output or a summarizer
  failure reports the raw tail as-is. `service.runSummarize` implements it over
  the concierge's LLM runner (45s bound, best-effort).
- ◻ **PWA "running commands" view** — open command tasks with a Watch shortcut
  (the task list shows them; watching is one tap away in the detail sheet).

**Scratchpad COMPLETE** — todos, schedules, run & watch, and completion
summaries all land.
