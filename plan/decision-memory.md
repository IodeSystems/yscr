# Subplan — decision memory & preference auto-resolve

> Record the choices the user makes; later questions whose answer is obvious
> from those preferences resolve automatically, with a log of what was decided
> and why.

## Purpose
The concierge keeps a durable **decision log** (what was chosen, for which
question, when, in which session). Before asking anything, it checks the log:
if a past decision implies the answer, it resolves automatically and records
the inference ("answered X because you chose Y on <date>, session Z").

## Requirements
1. **Capture** — every answered questionnaire (PWA tap AND concierge tool path)
   writes a decision entry: question text/hash, field→answer, source/session,
   timestamp. One table; both paths funnel through `handleAnswer`/the tool.
2. **Lookup + auto-resolve** — before surfacing a new questionnaire, match
   against the log (exact question hash first; LLM-judged "same preference" as
   a second pass, clearly labeled). Auto-resolved answers go through the SAME
   `Act` path and are logged with their provenance.
3. **The log is visible** — PWA: a decisions view ("what have I decided");
   conversational recall ("why did you pick EU?").

## Design constraints
- **Conservative by default:** auto-resolve only on exact/near-exact question
  match; LLM-judged matches are proposed, not applied (one-tap confirm).
- Decisions are append-only + supersede (latest wins per question-hash), never
  silently deleted.

## Status
✅ **core done.**

- ✅ **`decisions/` package** — `Decision`, `KeyFor` (sha256[:16] of normalized
  question+field; case/whitespace-stable), `Store` interface, `Resolve`
  (deterministic exact-match auto-resolve with provenance lines), `Mem` impl.
  Import-free of the rest of the repo (same posture as reports).
- ✅ **Postgres** — `decisions` table (partial index on question_key WHERE
  status='open'); `AddDecision` supersedes open rows for the same key
  (append-only + supersede, never deleted); `OpenDecision`, `ListDecisions`.
- ✅ **Capture (both paths)** — `service.logAnswers` records every field of an
  answered questionnaire; called from `handleAnswer` (PWA tap) AND via
  `Concierge.SetDecisionLog` hook after the tool-path `Act` succeeds. Context
  labels which path + source·session. Best-effort: logging never fails the
  answer.
- ✅ **Persona rule** — DefaultSystem: before re-asking a question that has
  been asked before, assume the recorded answer and say so in one line; only
  re-ask when the situation clearly differs. (LLM-judged near-match = proposed,
  not applied — conservative by default, per the design constraint.)
- ✅ **PWA + HTTP** — `GET /api/decisions`; "Decisions I remember" section
  (open decisions, newest first, capped 12), refreshed after tap-to-answer.

- ✅ **Conversational recall** — `list_decisions` concierge tool (WithDecisions;
  read-only, open decisions newest-first capped 25, optional substring filter)
  so "why did you pick EU?" is answered from the log with provenance.
- **Remaining (optional):** LLM-judged near-match auto-resolve as a distinct
  proposed-then-confirmed path (today the persona does this conversationally —
  no separate store entry).
