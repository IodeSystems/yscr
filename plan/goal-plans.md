# Subplan — goal plans: dependency graphs + work-around-ambiguity

> Decompose a goal into a task graph, release what isn't blocked, park
> ambiguity in an explicit open-questions queue, and keep working.

## Purpose
"Get me to X" → the concierge produces a plan (tasks + dependency edges),
works everything that isn't blocked on the user or another task, and maintains
a visible list of open questions it needs answered — never stalling on work it
could do anyway.

## Requirements
1. **Goal → plan** — LLM decomposes a goal into tasks with `deps: [task_ids]`
   (deterministic validation: acyclic, ids exist). Durable in the cue store.
2. **Release gate extends to deps** — `cue.Plan` gains "all deps done" as a
   third axis alongside status + capacity. Pure fn stays pure.
3. **Open-questions queue** — durable `open_questions` (question, context,
   since). The concierge parks ambiguity here instead of blocking; PWA shows it
   next to "Needs you"; answering one re-evaluates dependent tasks.
4. **Continue-independent-work behavior** — a persona rule + the gate: after
   parking a question, the model is told what's still releasable and continues.

## Design constraints
- `cue.Plan` is a pure function — extend it, don't fork it.
- The open-questions queue composes with the Questionnaire crux for structured
  questions; free-form ambiguity gets its own lighter shape.
- Diagram rendering of the graph lives in `plan/reports.md` (one renderer, two
  consumers).

## Status
◻ todo — not started.
