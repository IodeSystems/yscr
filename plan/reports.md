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
✅ core done (2026-08-23):
- `reports/` package: Diagram intermediate form + SVG renderer (left-to-right
  dep-layering, arrowheads, status colors, XML escaping). Renderers: TaskGraph
  (cue tasks + deps), FleetMap (sessions by status), StatusBoard (work list).
  Zero repo imports — adapters copy shapes in.
- Concierge tools (`concierge/report.go`): `render_diagram(kind)` returns the
  SVG in a `<diagram>` block; `write_report(topic, body)` persists markdown to
  `~/.yscr/reports/<ts>-<slug>.md` and returns the path. LLM picks kind/scope +
  writes the report body; rendering is deterministic (no model in the draw path).
- Service wiring (`service/report.go`): ReportState fns over the durable store
  (cue tasks + statuses, scratchpad board) + live fleet states.
- PWA: `renderReply` extracts `<diagram>` blocks from a concierge bubble and
  renders the SVG inline (styles.css .msg .diagram).

- ✅ **PWA reports listing** — `GET /api/reports` (newest-first .md artifacts) +
  `GET /api/reports/{name}` (base-name validated, no traversal); "Reports"
  section in the PWA, tap opens the markdown. A write_report artifact is now
  openable, not just a path in chat.

Remaining (icebox-grade):
- Mermaid-in-markdown as an alternative renderer for non-SVG surfaces.
- Diagram of the open-questions → task dependency links.
