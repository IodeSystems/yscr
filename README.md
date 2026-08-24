# yscr

**The conversational mediary for your tmux panes** ("yes sir"). One
conversation covers everything running in your terminal: read, scroll back
through, and operate any TUI in a pane; keep a scratchpad of tasks, todos, and
schedules; run and watch commands; produce diagrams and detailed reports on
request; and drive multi-step work toward completion — without ever leaving the
conversation or looking at a screen.

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
  session maps, flows) and detailed written reports from the state it already
  sees — the same data that drives the high-level voice, rendered deeply when
  you want to look at it.
- **Remembers your decisions.** Records the choices you make (with a log);
  later questions whose answer is already obvious from those preferences are
  resolved automatically, citing the decision they came from.

**One line:** *yscr makes your tmux panes conversational — a coworker who sees
every pane, talks to the medium it's in, keeps the work-list and schedule for
you, plans around ambiguity, draws what it's working on when asked, and
remembers what you've already decided.*

## Shape

The concierge is an [agentkit](https://github.com/IodeSystems/agentkit)
session on **corrallm** (the LLM endpoint; corrallm's own downstream model
adapters are its business), with audio via **oidio** (STT/TTS).

It drives every pane through one plugin contract — `source.Source` — with a
program **Adapter** per supported application:

| adapter | observe | spawn | act |
|---|---|---|---|
| **claude** | live pane + JSONL transcript tail | new tmux session | answer questions (verified keystroke protocol) |
| **terminal** | scrollback + pipe-pane stream | shell window (run commands) | — |

A new supported program = a new Adapter, no new tmux/source code.

**The crux — `source.Questionnaire`:** structured input requests from a
program (Claude's AskUserQuestion, any schema-shaped prompt) are rendered
*conversationally* by the handler model and parsed *back* into a
schema-validated structured answer — the user faces a conversation, never a
form.

## Status

Shipped: the `source.Source` plugin contract + program adapters (claude,
terminal) with live-pane adoption, question detect/answer (hook-primary,
pane-parse fallback), Watch (SSE live-tail) and Narrate (LLM delta summary →
TTS); the concierge on agentkit with a serialized, coalescing per-session
dispatch queue; scratchpad (tasks/todos/schedules + run & watch commands);
goal plans (dependency graphs over the cue release gate) + open-questions
queue; decision memory (log + auto-resolve); diagrams (SVG) and persisted
reports on request; medium-aware verbosity; ambient narration with a
materiality gate; installable PWA (chat, session cards, detail sheet, tap-to-
answer "Needs you", work list, reports, streaming STT via PCM worklet, web
push); Postgres durable store; systemd supervision.

On the way (see `plan/plan.md`): streaming STT live browser drive; LLM
near-match decision proposals; more diagram kinds.

> ⚠️ Early project in places. See `plan/plan.md` for current state, active
> work, and decisions.
