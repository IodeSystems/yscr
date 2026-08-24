# Subplan — audio & medium-awareness

> Verbosity tuned to the medium; ambient narration; streaming STT finished.
> The user will work on the audio layer later — this subplan keeps the contract
> honest until then.

## Purpose
The assistant is aware of its medium and tunes verbosity: text can be dense;
audio must be sparse (a few sentences, magnitude not recitation). Today the
medium never reaches the model.

## Requirements
1. **Medium hint in the contract** — `Converse` carries a per-turn channel hint
   (text | speech-out); the system prompt gains spoken-style rules ("≤2
   sentences when speaking; no lists, no code"). PWA sets it from speak-mode /
   input modality. Cheap; makes future audio a consumer of an existing contract.
2. **Narration materiality port** — distill L1 / utterance L2 materiality-gate /
   durable summary (the two-stage gate ported from an earlier design) for the voice progress
   channel (currently narrate re-LLMs every 8s delta unconditionally).
3. **Ambient auto-narration** — active sessions narrate without a per-session
   toggle; push delivery (`Notify`) when the phone isn't looking. Requires #2's
   materiality gate so it doesn't drone.
4. **Streaming STT live drive** — the browser mic→worklet path is the only
   unverified seam (backend loop verified). Drive it live, then optionally
   retire the batch path; show partials in the input box.

## Status
◐ in progress.

- ✅ **#1 medium hint in the contract** — `ConverseOn(ctx, session, msg, medium)`
  ("" | "text" | "speech"); the medium rides the dispatch queue so coalesced
  turns keep the LAST caller's channel; a speech turn gets a one-line prefix on
  the merged user message ("heard by voice — at most two short sentences; no
  lists, code, or ids"). `POST /api/converse` accepts `"medium"`; the PWA sends
  "speech" when the utterance was voiced OR speak-mode is on (the reply will be
  spoken). Text-only turns cost nothing. Tested: coalesced text+voice turn
  carries the hint.
- ✅ **#2 + #3 ambient auto-narration with the materiality port** (`service/ambient.go`).
  The two-stage gate (ported from an earlier design), adapted: **L1 distill** is
  deterministic (last meaningful line of the raw delta — no LLM call) and only
  advances the world-model rev when the snapshot CHANGES; **L2 materiality gate**
  speaks only on a real advance with one utterance in flight, so an unchanged
  session never re-fires (the drone dies). The LLM phrases each advance as one
  spoken sentence → SSE `narration` events (same shape as per-session narrate) +
  web-push `Notify`. Driven by the session watcher (`ambientSync` starts/stops
  narrations as sessions enter/leave). Ships OFF (`ambient.enabled`).
  Tests: L1 distill/noise, no-drone gate (20 identical ticks → 1 utterance),
  E2E loop (advance speaks, drone silent, second advance speaks). -race green.
- ◐ #4 streaming STT live drive — prototype landed; the browser mic→worklet path
  is the only unverified seam.
