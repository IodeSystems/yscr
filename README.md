# yscr

**The conversational mediary for terminal sessions** ("yes sir"). One
conversation covers everything running on your machine: read, scroll back
through, and operate any TUI in your tmux panes; keep a scratchpad of tasks,
todos, and schedules; run and watch commands; produce diagrams and detailed
reports on request; and drive multi-step work toward completion — without ever
leaving the conversation or looking at a screen.

Extracted from [autowork3](https://github.com/IodeSystems/autowork3) so the
concierge (and its Claude Code transport) lives as a personal tool, separate
from the harness.

## Purpose

yscr sits between you and the programs in your panes (Claude Code CLIs, shells,
anything with scrollback) and translates both directions:

- **Coworker voice, medium-aware.** Reports high-level by default; detail only
  when asked, or when it matters — a KPI moved, was added, or is at risk.
  Verbosity is tuned to the *medium*: text can be dense; audio must be sparse
  (the audio layer lands later, but the contract assumes it now).
- **Reads and operates.** Pulls live output and scrollback on demand; answers
  prompts, submits inputs, steers sessions — through conversation.
- **Manages horizon work.** Keeps a durable scratchpad — tasks, todos,
  schedules/cron — and runs terminal commands in the foreground or background,
  watching them and reporting what's material.
- **Plans toward goals.** Decomposes a goal into a plan with dependencies,
  works everything that isn't blocked, and parks ambiguity in an explicit queue
  of open questions for you — never stalling on what it could be doing anyway.
- **Shows its work.** On request, produces diagrams (dependency graphs,
  system/session maps, flows) and detailed written reports from the state it
  already sees — the same data that drives the high-level voice, rendered
  deeply when you want to look at it.
- **Remembers your decisions.** Records the choices you make (with a log);
  later questions whose answer is already obvious from those preferences are
  resolved automatically, citing the decision they came from.

**One line:** *yscr makes your terminal sessions conversational — a coworker
who sees every pane, talks to the medium it's in, keeps the work-list and
schedule for you, plans around ambiguity, draws what it's working on when
asked, and remembers what you've already decided.*

## Shape

The concierge is an [agentkit](https://github.com/IodeSystems/agentkit)
session with a **swappable LLM endpoint** (corrallm / OpenRouter / Claude Code
CLI in a tmux virtual terminal) and audio via **oidio** (STT/TTS) ↔ corrallm.

It drives every backend through one plugin contract — `source.Source`:

| plugin | observe | spawn | act |
|---|---|---|---|
| **autowork** | fleet rollup + event feed (via autowork3 API) | new thread/issue | apply-decision, confirm-send |
| **pane: claude** | live pane + JSONL transcript tail | new tmux session | answer questions (verified keystroke protocol) |
| **pane: terminal** | scrollback + pipe-pane stream | — | — |
| **openai** | conversation stream (corrallm/OpenRouter) | new conversation | — |

**The crux — `source.Questionnaire`:** structured input requests (MCP tool
schemas, autowork decision_requests, quizzes) are rendered *conversationally*
by the handler model and parsed *back* into a schema-validated structured
answer — the user faces a conversation, never a form.

## Status

Shipped: the `source.Source` plugin contract; concierge on agentkit with a
serialized, coalescing per-session dispatch queue; pane source (tmux plumbing +
program adapters for claude and terminal) with live-pane adoption, question
detect/answer (hook-primary, pane-parse fallback), Watch (SSE live-tail) and
Narrate (LLM delta summary → TTS); installable PWA (chat, fleet cards, detail
sheet, tap-to-answer "Needs you", streaming STT via PCM worklet, web push);
Postgres durable conversation store; the cue system (LLM generator → durable
task queue → deterministic release gate under capacity rails → completion
reconcile), shipping off/dry-run.

On the way (see `plan/plan.md`): user-facing scratchpad (tasks/todos/schedules)
and run+watch commands as concierge tools; goal plans with dependency graphs
extending the cue release gate; an open-questions queue with
continue-independent-work behavior; medium-aware verbosity hints; decision
memory with auto-resolve and a decision log; diagram + detailed-report
generation on request.

> ⚠️ Early scaffold in places. See `plan/plan.md` for current state, active
> work, and decisions.
