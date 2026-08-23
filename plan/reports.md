# Subplan — diagrams & detailed reports on request

> The same state that drives the high-level voice, rendered deeply when the
> user wants to look at it.

## Purpose
On request ("draw the dependency graph", "give me a full report on session X"),
the concierge produces:
1. **Diagrams** — task/dependency graphs, session/fleet maps, flows. Rendered
   as SVG (PWA-native) from the structured state it already has (cue tasks +
   deps, `source.State` rollups). Mermaid-in-markdown is an acceptable interim.
2. **Detailed reports** — long-form written reports: per-session deep dive
   (transcript/scrollback via `read_history`), fleet status report, goal-
   progress report. Persisted to disk (`~/.yscr/reports/`) + linked in the PWA,
   so a report is an artifact, not just chat text.

## Requirements
1. Concierge tools: `render_diagram(kind, ...)` and `write_report(topic, ...)`.
2. Deterministic renderers (no LLM in the drawing path — the LLM picks kind +
   scope; the renderer draws from validated state).
3. PWA surfaces reports as openable artifacts; diagrams inline in chat.

## Design constraints
- **LLM proposes, deterministic layer renders** — same line as cue/scratchpad.
- One diagram renderer serves goal-plans (dep graphs) and fleet maps.

## Status
◻ todo — not started.
