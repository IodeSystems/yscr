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
◻ todo — not started.
