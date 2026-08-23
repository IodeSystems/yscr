> _Ported from autowork3 `plan/` (2026-07-12) — design history for the concierge/membrane before it was extracted into this repo. Paths like `internal/…` and table names are autowork3-internal, as-built when this lived in AW. The Android client described here was never shipped in yscr, which uses a PWA (`web/`) instead; kept for design reference._

# Android voice client — native client for the chat membrane

Status: design 2026-07-02. Owner: Carl. Sibling of `ui/`; talks to the
daemon over HTTP/SSE + the shipped audio proxy. Fronts the concierge
(per-thread today; **fleet_concierge** once `plan/conversational-membrane.md`
step 1.5 lands). One-line how-this-works: pick up from Active work; each
slice carries next / risks / blocking decisions.

## What already exists (grounding, 2026-07-02)

- **Audio proxy is shipped** — `internal/server/audio_proxy.go`:
  `GET /api/audio/capabilities`, `POST /api/audio/transcriptions`
  (multipart STT, 25 MiB cap), `POST /api/audio/speech`
  (`{model,input,voice}` → binary). Forward-only, corrallm base+key
  resolved server-side (`ResolveAudioTarget`), inbound `Authorization`
  stripped. **Realtime `/v1/realtime` proxy is NOT built** (plan-only).
- **Every op is triple-exposed** via Huma+gat: REST `/api/…`,
  Connect-RPC `/rpc/Autowork3.v1.Service/<Method>`, GraphQL `/graphql`
  — same handler. Client ops we need: `listThreads`, `getThread`,
  `listThreadEvents`, `sendMessage` (`POST /api/threads/{tid}/messages`,
  body `{content}`), `converseThread`
  (`POST /api/threads/{tid}/converse` → `{child_thread_id,
  concierge_session_id}`), `submitDecision`, `confirmSend`
  (`.../send-pending/{id}/confirm`).
- **Live updates = SSE only, per-thread**: `GET /api/threads/{tid}/stream`,
  payload `data: {"type","data","timestamp"}`, `: keepalive` every 30s.
  It's a **coarse invalidation nudge** — the web client refetches via
  GraphQL on each event (`ui/src/api/sse.ts`). No client WebSocket, no
  GraphQL subscription.
- **Web voice client already exists** to mirror: `ui/src/api/voice.ts`
  (`getAudioCapabilities` / `transcribe` / `synthesize` / `converseThread`
  / `confirmSendPending`), `ui/src/api/useVoice.ts` (MediaRecorder
  push-to-talk), `ui/src/components/TalkButton.tsx`.
- **Repo layout**: `android/` sits top-level beside `ui/`, standalone
  Gradle, NOT `go:embed`-ed (the UI isn't either — prod serves a
  placeholder / on-disk `$AW_HOME/public`). Optional `android-*` Makefile
  targets to match the `ui-*` convention.

## ✅ Auth — client bearer token (BUILT 2026-07-02, server side)

The step-0 blocker is **resolved on the daemon side.** `internal/server/auth.go`:
a `client_tokens` DB table (id/name/sha256-hex) + an in-memory hash set +
`clientAuthMiddleware` registered on the chi router. Gate =
**loopback | same-origin | valid Bearer token** (`clientRequestAllowed`,
shared with the audio-proxy gate). Exempt: `/healthz` `/git/` `/ws/`
`/dist/` `/install.sh` `/schema/`. RPCs `issueClientToken` /
`listClientTokens` / `revokeClientToken` + CLI `aw token issue|list|revoke`
(bootstrap over loopback needs no token; the raw token is shown once, only
its hash stored). Sink of truth is the DB; the running daemon honors
issue/revoke immediately (in-memory set refreshed). Tested (exempt/loopback/
same-origin/token matrix, issue→validate→revoke, middleware 401);
testharness e2e still green (loopback unaffected).

**Behavior change:** `/api` `/rpc` `/graphql` (+ SSE, the embedded UI at
`/`) went from open-on-all-interfaces to gated. Web UI (same-origin) + CLI
(loopback) unchanged; a remote **non-browser** client now needs a token —
which is exactly the phone. NOTE (unchanged from the model decision):
same-origin browsers are still trusted, so a remote browser loading the UI
is NOT blocked — hardening that is a separate UI-login change.

**App side (still TODO, now unblocked):** `aw token issue --name phone` →
store the token in Android Keystore → send `Authorization: Bearer <token>`
on every daemon call (RPC + `/api/audio/*` + SSE).

## (historical) The blocker — auth (was step 0)

The daemon has **no client authentication**. Security today is
**loopback-only**; `api.go` binds `:<port>` (all interfaces) and the
audio-proxy comment explicitly calls an all-interfaces bind "an
unauthenticated open relay." The audio proxy is gated by **loopback OR
same-origin browser signals** (`Sec-Fetch-Site`/`Origin`,
`audio_proxy.go:99`). **An Android app is neither loopback nor a
browser → hard 403 on `/api/audio/*`**, and the rest of the API would be
wide open if LAN-exposed.

The web UI dodges all of this by being same-origin. A phone cannot. So
the Android client **forces** the auth question. Options:

| # | approach | cost | verdict |
|---|---|---|---|
| A | **Client bearer token** — issue a token (`aw token issue` / config), require it on `/api/*` for non-loopback, and accept loopback OR same-origin OR valid token in the audio-proxy gate | moderate, server-side | **Recommended.** Precedent exists: git-proxy already does hashed bearer auth (`git_proxy.go:140`), worker-registration issues a one-time bearer. This is the enabling primitive for ANY non-browser client. |
| B | **Tailscale/WireGuard** private net — phone reaches daemon over a trusted overlay | low, no code | Transport privacy only; the phone still isn't loopback and isn't same-origin, so the audio-proxy gate STILL 403s. Doesn't remove the need for A. Good *complement* to A. |
| C | adb reverse / SSH port-forward to present loopback | trivial | Dev-only, not a product. |

**DECIDED 2026-07-02: A (client bearer token).** Tailscale optional later
for network privacy; not required to unblock. Everything below assumes A.

## Client architecture — Kotlin + Jetpack Compose (DECIDED 2026-07-02)

- **Transport → Connect-RPC via `connect-kotlin`.** The `/rpc` surface
  already exists; generate typed Kotlin stubs from the same
  FileDescriptorSet the Go client uses (`/schema/proto` / `cmd/proto-dump`).
  Typed, efficient (proto on the wire), reuses existing codegen. **Audio
  is REST-only** regardless (`/api/audio/*`) — call it with OkHttp
  directly. *Fallback:* skip RPC, hit the handful of REST ops with
  OkHttp + kotlinx.serialization (simplest, no codegen) — fine for an MVP.
- **Config**: the web client hard-assumes relative paths / same-origin.
  A phone needs an **absolute, configurable base URL** (LAN IP / Tailscale
  name) + the **bearer token**, stored in Android Keystore /
  EncryptedSharedPreferences.
- **Live updates**: OkHttp SSE (`okhttp-sse`) on the concierge thread's
  `/stream`; treat each event as an invalidation nudge → refetch via RPC.
  Mirror `ui/src/api/sse.ts`. Don't build a WS path (none exists).
- **Voice loop (turn-based v1, push-to-talk — mirrors `useVoice.ts`)**:
  1. Capture: `AudioRecord`/`MediaRecorder` → clip (proxy accepts
     multipart; stay well under 25 MiB).
  2. STT: multipart POST `/api/audio/transcriptions` → `{text}`.
  3. Transcript → **editable box** → `sendMessage` to the concierge
     thread (mirror web: never auto-send the raw transcript).
  4. Reply: SSE nudge → fetch the concierge reply event → POST
     `/api/audio/speech` `{model,input,voice}` → play via
     `ExoPlayer`/`MediaPlayer`.
  5. **Staged-send read-back** (safety): on a `send_pending` event, TTS
     the verbatim body + surface `pending_id`; spoken "confirm" →
     `confirm_send` (→ `voice_confirmed` provenance) or a tap → REST
     confirm (→ `human`). Reuses the shipped safety substrate — no new
     safety surface.
- **Efficiency**: OkHttp connection reuse; proto payloads; SSE not
  polling; foreground service only while a voice session is live;
  push-to-talk (no always-on VAD in v1) → battery-friendly.

## Notifications (native app) — design 2026-07-02

The app wants **push notifications**, each leading to a **text summary**
with three actions: **say it**, **directly respond**, or **see the
detailed report**. This maps almost entirely onto what already exists.

**Reuse — the outbound-push path is already built.** autowork3 has
`EventAlert` → `scheduler.reconcileAlerts` → **ntfy** POST
(`internal/server/notifier.go`): retry window, secret-resolved `token_ref`,
`ClickURL`, `Priority`, off the request path via `alertLoop`. ntfy also
supports notification **`actions`** (view / http / broadcast). So the
notification substrate is done; the new work is (a) *emitting* an
EventAlert for YSCR-worthy events and (b) the app rendering the actions.

**The text summary is already generated.** The alert Body = the YSCR
utterance (S8d) or the digest notice (`yscrNoticeSummary`, S5). The
`fleet_summary` (S8b) is the drill-down text. Nothing new to compute.

**The three actions:**

| action | how | reuses |
|---|---|---|
| **text summary** | the notification Body | YSCR utterance (S8d) / notice (S5) |
| **say it** | app-local: POST the body to `/api/audio/speech` → play | shipped audio proxy |
| **directly respond** | deep-link into the app's YSCR compose (push-to-talk or text) → `sendMessage` to the yscr thread | ConverseYSCR + sendMessage |
| **see detailed report** | deep-link to the source thread's detail view → `pull_thread_detail` / thread events (source thread id rides in the alert metadata) | fleet tools / thread events |

On Android these are the notification's action buttons (ntfy `view`/
`broadcast` intents) + an app-local "say it"; the Body is the summary.

**App side ✅ SCAFFOLDED 2026-07-02:** `android/NotificationsService.kt` —
a foreground service subscribes to the ntfy topic stream (`{ntfy}/json`;
topic set in Settings, = `webapi.yscr_notify`) and raises a notification
per alert with **Say it** (TTS body via audio proxy) / **Respond** (open
YSCR chat) / **Report** (open fleet); actions deep-link through MainActivity
intent extras. POST_NOTIFICATIONS + FGS(dataSync) permissions;
`Settings → Enable notifications`. Compile-verified. Production hardening =
FCM / UnifiedPush (a held FGS is battery-heavy + reclaimable).

**S-notify (autowork3-side)** ✅ (2026-07-02, server hook shipped)
`emitYSCRAlert` (yscr.go) queues one `EventAlert{title:"Fleet update",
body:<summary>, click_url:/yscr/<thread>, url/token_ref/priority}` via the
existing ntfy delivery path (`reconcileAlerts`). Wired into
`injectYSCRNarrationNudge` — **one push per material fleet delta** (same L2
gate as the in-app narration, so no firehose). No-op unless a sink is
configured. Sink config = `webapi.yscr_notify` in config.json (JSON blob,
schedules.notify shape: `{"url","token_ref","priority"}`) →
`cfg.WebAPI.YSCRNotify` → `srv.yscrNotifyJSON`, parsed per-alert into
`notifySink`. `token_ref` is a secret NAME, resolved at send time — no
token material in events. Tested: `TestEmitYSCRAlert` (sink on/off, body +
deep link + secret-name), `TestYSCRUtterance_PushesAlert` (wiring). **App
side still TODO** (gated on auth): subscribe to the sink, render Body +
the 3 action buttons. Push transport decision (ntfy-reuse vs FCM) still
open below.

**Blocking decision (Carl owns):** push transport —
- **ntfy (reuse)** — the app subscribes to an ntfy topic (native ntfy
  protocol or the ntfy Android app); zero new server infra, actions
  supported. Recommended to start.
- **FCM (Firebase)** — the "native" path; needs a device-token registry +
  FCM sender (new infra). Better background reliability; more setup.
Recommend ntfy-reuse first, FCM later if background delivery needs it.

## Build order

0. **Daemon client-auth token** (option A) + audio-proxy gate accepts it.
   ✅ DONE 2026-07-02 (server side) — see the "Auth" section above.
1. **Target = `fleet_concierge` (DECIDED 2026-07-02).** The phone waits on
   `plan/conversational-membrane.md` step 1.5 (fleet server layer). Steps 0
   (auth) and the fleet layer are both server-side and independent → they
   run in parallel; the phone starts once both land.
2. **Android skeleton** (text-first): ✅ SCAFFOLDED 2026-07-02 in `android/`
   — Kotlin + Compose, REST + OkHttp (org.json), targets API 35. Settings
   (host+token) → Fleet (`GET /api/threads`) → Chat (`POST /api/yscr/converse`
   + `POST .../messages` + `GET .../events`). Every call sends
   `Authorization: Bearer`. **Compile-verified: `./gradlew assembleDebug`
   builds a 9.6M app-debug.apk** on this box (SDK at ~/Android/Sdk, JDK 21,
   Gradle wrapper 8.10.2, AGP 8.6.1). Not yet run (needs device/emulator).
   **SSE live feed wired 2026-07-02** (`okhttp-sse`): chat auto-updates on
   each thread event, `yscr_status` shows the narration phase in the header,
   2s reconnect on drop. Rebuild green. Next: audio push-to-talk. See
   `android/README.md`.
3. **Audio turn-based**: push-to-talk STT → editable transcript → send;
   reply → TTS → play. Mirror `useVoice.ts`.
4. **Staged-send read-back** + `voice_confirmed` through `confirmSend`.
5. **(defer) Realtime v2** — needs the `/v1/realtime` SDP proxy built
   first (autowork3-side, plan-only). WebRTC `RTCPeerConnection` to
   corrallm, media P2P, signaling proxied. Turn-based proves the loop.

## Decisions (2026-07-02)

- **Auth** — ✅ client bearer token (option A). Tailscale optional later.
- **Stack** — ✅ Kotlin + Jetpack Compose native.
- **Target** — ✅ `fleet_concierge` (phone waits on membrane step 1.5).
- **Transport** — OPEN: Connect-RPC (`connect-kotlin`, typed, reuses proto)
  vs plain REST+OkHttp (simplest MVP). Recommend RPC; REST acceptable to
  start. Low-stakes — defer to skeleton slice.

## Risks / untested

- The audio-proxy same-origin gate change (step 0) must not re-open the
  relay for browsers — keep loopback+same-origin, ADD token; don't replace.
- corrallm STT clip format/codec the proxy forwards to parakeet is
  untested from Android (`AudioRecord` WAV vs `MediaRecorder` m4a) — verify
  against `/v1/capabilities` before committing a codec.
- SSE reconnect/backoff on mobile network flaps is on us (OkHttp SSE has
  no auto-resume of the event log — the daemon has no cursor/replay).
