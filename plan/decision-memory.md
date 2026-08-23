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
◻ todo — not started.
