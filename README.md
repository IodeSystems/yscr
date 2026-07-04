# yscr

**Standalone fleet concierge** ("yes sir") — a personal, voice-first membrane
that observes and drives work across heterogeneous **session sources**.

Extracted from [autowork3](https://github.com/IodeSystems/autowork3) so the
concierge (and its Claude Code transport) lives as a personal tool, separate
from the harness.

## Shape

The concierge is an [agentkit](https://github.com/IodeSystems/agentkit)
session with a **swappable LLM endpoint** (corrallm / OpenRouter / Claude Code
CLI in a tmux virtual terminal) and audio via **oidio** (STT/TTS) ↔ corrallm.

It drives every backend through one plugin contract — `source.Source`:

| plugin | observe | spawn | act |
|---|---|---|---|
| **autowork** | fleet rollup + event feed (via autowork3 API) | new thread/issue | apply-decision, confirm-send |
| **claude-code** | tmux session stream | new tmux session | — |
| **openai** | conversation stream (corrallm/OpenRouter) | new conversation | — |

**The crux — `source.Questionnaire`:** structured input requests (MCP tool
schemas, autowork decision_requests, quizzes) are rendered *conversationally*
by the handler model and parsed *back* into a schema-validated structured
answer — the user faces a conversation, never a form.

> ⚠️ Early scaffold. The `source` plugin contract is the first thing landed;
> the concierge, plugins, and service wiring follow. See `plan/plan.md`.
