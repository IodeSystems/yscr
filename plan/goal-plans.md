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
✅ core done — see below. ◻ remaining: goal-plan tool (batch decompose into a
named plan), PWA dep-graph view (deferred to reports.md).

### ✅ Dependency axis on the release gate (`cue/cue.go`)
- `Task.Deps []string` — ids that must be DONE before the task releases.
- `PlanWithStatus(tasks, fleet, inflight, caps, releasable, done, live)` — the
  third axis: a pending task holds ("waiting on <id>" / "dep <id> not found")
  until every dep is done. `Plan` unchanged (nil status = no deps consulted).
- `ValidateDeps(tasks)` — pure acyclic + known-id check for a plan batch.
- 4 new tests (hold-until-done, missing dep, nil-status ignores deps, validate).

### ✅ Durable deps (`store/pg.go`)
- `cue_tasks.deps` column (comma-joined; ALTER … IF NOT EXISTS migrates);
  EnqueueTask/queryTasks round-trip it.
- `TaskStatuses(ctx)` → (done, live maps by id) for the gate.
- Store round-trip test: enqueue with deps → held until dep done → released.

### ✅ Generator proposes edges (`service/cuegen.go`)
- Proposals carry `id` + `deps`; the task ID is the model's id (stable within a
  batch so edges resolve). Deterministic validation before enqueue: unknown
  deps dropped (task still enqueues), cycles relaxed edge-by-edge via
  `ValidateDeps` — work is never lost, only bad edges are. 3 new tests.

### ✅ Release loop uses the gate (`service/cue.go`)
- `cueRunner.release` pulls `TaskStatuses` and calls `PlanWithStatus` — deps
  hold alongside status + capacity. cueStore interface gained TaskStatuses.

### ✅ Open-questions queue (`questions/` pkg + store + concierge + PWA)
- `questions.Question{ID, Question, Context, TaskID, Status, Answer}`;
  `QuestionsStore{Add, List, Answer}` (named to avoid PG's Add collision);
  Mem impl. Postgres `open_questions` table (partial unique index on open text
  → parking twice is a no-op).
- Concierge tools: `ask_question` (park + "continue with the work that does not
  depend on it"), `list_questions`, `answer_question`. Persona rule added to
  DefaultSystem: work around ambiguity, never stall, never guess at a decision
  the user owns.
- PWA "I need from you" section (`#openq`): tap-to-answer input (POST
  /api/questions/{id}/answer — no LLM) + GET /api/questions.

### ◻ Remaining
- **Goal-plan tool** — `plan_goal(goal)` that batch-decomposes into a named
  task graph in one call (today the model composes plans from add_task + the
  generator; a first-class plan shape with a goal id is the clean upgrade).
- PWA dep-graph rendering → `plan/reports.md` (one renderer, two consumers).
