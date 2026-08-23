# yscr — icebox (deferred, opt-in)

- Priority decay / deadlines on cued tasks; per-source routing policy.
- Retire the batch STT path once streaming is proven on-device; show partials in the input box.
- Optional auth for the service (LAN-only, deferred).
- Android native client (Kotlin + Jetpack Compose) — see `plan/archive/android-voice-client.md`; PWA covers it today.
- openai plugin: Historian (History) + streaming Observe; claude multi-line Post submits via paste buffer (`send()` fix).
