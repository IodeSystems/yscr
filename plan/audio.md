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
   durable summary from autowork3's `yscr_status.go` for the voice progress
   channel (currently narrate re-LLMs every 8s delta unconditionally).
3. **Ambient auto-narration** — active sessions narrate without a per-session
   toggle; push delivery (`Notify`) when the phone isn't looking. Requires #2's
   materiality gate so it doesn't drone.
4. **Streaming STT live drive** — the browser mic→worklet path is the only
   unverified seam (backend loop verified). Drive it live, then optionally
   retire the batch path; show partials in the input box.

## Status
◐ in progress — #1 not started; #2 not started; #3 not started; #4 prototype
landed, needs the live browser drive.
