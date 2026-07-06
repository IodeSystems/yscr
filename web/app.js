// YSCR PWA client. Talks to the yscr service; registers a service worker for
// background + push notifications.

const $ = (s) => document.querySelector(s);
const api = (p, opts) => fetch(p, opts).then((r) => (r.ok ? r : Promise.reject(new Error(r.status))));

// ── activity status line (recording / transcribing / thinking) ──────
// A single-line indicator above the composer. kind drives the dot color +
// pulse; text is the label. setStatus(null) hides it.
function setStatus(text, kind) {
  const el = $("#status");
  if (!text) {
    el.hidden = true;
    el.textContent = "";
    return;
  }
  el.hidden = false;
  el.dataset.kind = kind || "work";
  el.innerHTML = `<span class="dot"></span>${escape(text)}`;
}

// ── conversation ────────────────────────────────────────────────────

function bubble(text, cls) {
  const el = document.createElement("div");
  el.className = "msg " + cls;
  el.textContent = text;
  $("#log").append(el);
  el.scrollIntoView({ block: "end" });
  return el;
}

async function send(message, voice) {
  bubble(message, voice ? "you voice" : "you");
  const pending = bubble("", "yscr thinking");
  pending.innerHTML = '<span class="typing"><i></i><i></i><i></i></span>';
  setStatus("Thinking…", "think");
  try {
    const r = await api("/api/converse", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ message }),
    });
    const { reply } = await r.json();
    pending.classList.remove("thinking");
    pending.textContent = reply || "(no reply)";
    if (speakOn && reply) speak(reply);
  } catch (e) {
    pending.classList.remove("thinking");
    pending.className = "msg err";
    pending.textContent = "error: " + e.message;
  } finally {
    idleStatus();
  }
  loadFleet();
}

// ── fleet ───────────────────────────────────────────────────────────

async function loadFleet() {
  const box = $("#fleet");
  try {
    const r = await api("/api/fleet");
    const { sessions } = await r.json();
    box.innerHTML = "";
    if (!sessions || !sessions.length) {
      box.innerHTML = '<div class="empty">Nothing active across any source.</div>';
      return;
    }
    for (const s of sessions) {
      const card = document.createElement("div");
      card.className = "card";
      const pending = (s.Pending || []).length
        ? `<div class="pending">${s.Pending.length} decision(s) awaiting you</div>`
        : "";
      card.innerHTML = `
        <div class="top">
          <span class="title">${escape(s.Ref.Title || s.Ref.ID)}</span>
          <span class="status ${s.Status}">${s.Status}</span>
        </div>
        <div class="summary">${escape(s.Ref.Source)} · ${escape(s.Summary || "")}</div>
        ${pending}`;
      box.append(card);
    }
  } catch (e) {
    box.innerHTML = `<div class="empty">fleet unavailable (${e.message})</div>`;
  }
}

function escape(s) {
  const d = document.createElement("div");
  d.textContent = s == null ? "" : String(s);
  return d.innerHTML;
}

// ── push notifications ──────────────────────────────────────────────

function urlBase64ToUint8Array(b64) {
  const pad = "=".repeat((4 - (b64.length % 4)) % 4);
  const raw = atob((b64 + pad).replace(/-/g, "+").replace(/_/g, "/"));
  return Uint8Array.from([...raw].map((c) => c.charCodeAt(0)));
}

async function enablePush() {
  const btn = $("#enable-push");
  if (!("serviceWorker" in navigator) || !("PushManager" in window)) {
    alert("Push not supported in this browser.");
    return;
  }
  const perm = await Notification.requestPermission();
  if (perm !== "granted") return;
  const reg = await navigator.serviceWorker.ready;
  const { public_key } = await api("/api/push/vapid").then((r) => r.json());
  const sub = await reg.pushManager.subscribe({
    userVisibleOnly: true,
    applicationServerKey: urlBase64ToUint8Array(public_key),
  });
  await api("/api/push/subscribe", {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify(sub),
  });
  btn.classList.add("on");
  btn.title = "Notifications enabled";
}

// ── voice: mic (STT) + speak (TTS) ──────────────────────────────────

let audioCfg = { stt_model: "", tts_model: "", tts_voice: "" };
let speakOn = false;

// Hands-free VAD listening state. Toggle the mic on and it listens continuously:
// each trailing pause auto-finalizes an utterance (transcribe + send) and it
// keeps listening for the next turn until toggled off.
let listening = false;       // continuous listen session active
let micStream = null;        // persistent mic stream for the session
let audioCtx = null, analyser = null, vadRAF = 0;
let segRec = null, segChunks = [], segMime = ""; // current utterance recorder
let finalizeSend = false;    // set right before segRec.stop() to send-or-drop
let speaking = false;        // TTS playing → suppress capture (echo/self-trigger)
let hadSpeech = false, speechStart = 0, silenceStart = 0;

// VAD tunables: RMS energy threshold to count as voice, trailing silence (ms)
// that ends an utterance, and the minimum voiced span to send (drops clicks).
const VAD = { threshold: 0.018, silenceMs: 900, minSpeechMs: 250 };

// idleStatus restores the resting indicator: "Listening…" while a hands-free
// session is open, otherwise hidden.
function idleStatus() { setStatus(listening ? "Listening…" : null, "rec"); }

// One persistent element, unlocked inside a user gesture (iOS Safari requires
// user-activation to play audio; a later async .play() on an already-unlocked
// element is allowed).
const ttsAudio = new Audio();

async function loadAudioConfig() {
  try {
    audioCfg = await api("/api/audio/config").then((r) => r.json());
    $("#mic").style.display = "";
    $("#speak").style.display = "";
  } catch (_) {
    // audio disabled server-side → hide the controls
    $("#mic").style.display = "none";
    $("#speak").style.display = "none";
  }
}

// extForMime maps a MediaRecorder mimeType to a matching filename extension
// (corrallm content-sniffs, but keep the name honest across browsers).
function extForMime(mime) {
  if (!mime) return "webm";
  if (mime.includes("mp4") || mime.includes("m4a") || mime.includes("aac")) return "m4a";
  if (mime.includes("ogg")) return "ogg";
  if (mime.includes("wav")) return "wav";
  return "webm";
}

async function startListening() {
  if (listening) return;
  try {
    micStream = await navigator.mediaDevices.getUserMedia({ audio: true });
  } catch (e) {
    console.warn("mic denied", e);
    $("#mic").classList.remove("on");
    return;
  }
  const AC = window.AudioContext || window.webkitAudioContext;
  if (!AC) { // no VAD available in this browser → don't half-start
    micStream.getTracks().forEach((t) => t.stop());
    micStream = null;
    $("#mic").classList.remove("on");
    return;
  }
  listening = true;
  $("#mic").classList.add("on");
  setStatus("Listening…", "rec");
  audioCtx = new AC();
  const src = audioCtx.createMediaStreamSource(micStream);
  analyser = audioCtx.createAnalyser();
  analyser.fftSize = 1024;
  src.connect(analyser);
  hadSpeech = false;
  silenceStart = 0;
  startSegment();
  monitorVAD();
}

function stopListening() {
  listening = false;
  if (vadRAF) { cancelAnimationFrame(vadRAF); vadRAF = 0; }
  const rec = segRec;
  segRec = null;
  if (rec && rec.state !== "inactive") { rec.onstop = null; rec.stop(); } // drop trailing
  if (micStream) { micStream.getTracks().forEach((t) => t.stop()); micStream = null; }
  if (audioCtx) { audioCtx.close().catch(() => {}); audioCtx = null; }
  analyser = null;
  $("#mic").classList.remove("on");
  setStatus(null);
}

// startSegment opens a fresh recorder for the next utterance. Its onstop sends
// the captured audio (when finalizeSend) and immediately reopens the next
// segment so listening is continuous.
function startSegment() {
  if (!micStream) return;
  segChunks = [];
  segRec = new MediaRecorder(micStream);
  segMime = segRec.mimeType;
  segRec.ondataavailable = (e) => e.data.size && segChunks.push(e.data);
  segRec.onstop = () => {
    const chunks = segChunks, mime = segMime;
    if (finalizeSend && chunks.length) {
      const blob = new Blob(chunks, { type: mime || "audio/webm" });
      if (blob.size) transcribeAndSend(blob, extForMime(mime)); // async; keeps listening
    }
    finalizeSend = false;
    if (listening) startSegment();
  };
  segRec.start();
}

// endUtterance stops the current segment; onstop transcribes+sends (if send) and
// reopens the next segment.
function endUtterance(send) {
  finalizeSend = send;
  if (segRec && segRec.state !== "inactive") segRec.stop();
}

// monitorVAD polls RMS energy and endpoints on a trailing pause. Capture during
// TTS playback is suppressed (speaking) so the concierge's own voice never
// triggers a turn.
function monitorVAD() {
  const buf = new Uint8Array(analyser.fftSize);
  const tick = () => {
    if (!listening || !analyser) return;
    analyser.getByteTimeDomainData(buf);
    let sum = 0;
    for (let i = 0; i < buf.length; i++) { const v = (buf[i] - 128) / 128; sum += v * v; }
    const rms = Math.sqrt(sum / buf.length);
    const now = performance.now();
    const voiced = rms > VAD.threshold && !speaking;
    if (voiced) {
      if (!hadSpeech) { hadSpeech = true; speechStart = now; setStatus("Recording…", "rec"); }
      silenceStart = 0;
    } else if (hadSpeech) {
      if (!silenceStart) silenceStart = now;
      else if (now - silenceStart > VAD.silenceMs) {
        const spoke = silenceStart - speechStart > VAD.minSpeechMs;
        hadSpeech = false;
        silenceStart = 0;
        if (spoke) setStatus("Transcribing…", "work");
        else idleStatus();
        endUtterance(spoke); // onstop sends (if spoke) + reopens the next segment
      }
    }
    vadRAF = requestAnimationFrame(tick);
  };
  vadRAF = requestAnimationFrame(tick);
}

async function transcribeAndSend(blob, ext) {
  setStatus("Transcribing…", "work");
  const fd = new FormData();
  fd.append("file", blob, "speech." + (ext || "webm"));
  if (audioCfg.stt_model) fd.append("model", audioCfg.stt_model);
  try {
    const r = await api("/api/audio/transcriptions", { method: "POST", body: fd });
    const data = await r.json();
    const text = (data.text || "").trim();
    if (text) send(text, true); // voice-flagged; send() takes over the status
    else idleStatus();
  } catch (e) {
    idleStatus();
    bubble("transcription failed: " + e.message, "err");
  }
}

async function speak(text) {
  try {
    const r = await api("/api/audio/speech", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ input: text, model: audioCfg.tts_model, voice: audioCfg.tts_voice || undefined }),
    });
    const buf = await r.blob();
    const url = URL.createObjectURL(buf);
    setSpeaking(true); // suppress VAD capture while the reply plays
    ttsAudio.onended = () => { URL.revokeObjectURL(url); setSpeaking(false); };
    ttsAudio.onerror = () => setSpeaking(false);
    ttsAudio.src = url;
    ttsAudio.play().catch((e) => { console.warn("tts play failed", e); setSpeaking(false); });
  } catch (e) {
    console.warn("tts fetch failed", e);
    setSpeaking(false);
  }
}

// setSpeaking gates hands-free capture during TTS: it pauses the live segment
// recorder so the concierge's own voice isn't recorded/transcribed, and resumes
// (with a fresh silence window) when playback ends.
function setSpeaking(v) {
  speaking = v;
  if (!segRec) return;
  if (v && segRec.state === "recording") segRec.pause();
  if (!v && segRec.state === "paused") { hadSpeech = false; silenceStart = 0; segRec.resume(); idleStatus(); }
}

// Tap to toggle hands-free listening: tap on and it listens continuously,
// auto-sending each utterance on a trailing pause (VAD); tap off to stop.
const mic = $("#mic");
mic.addEventListener("click", () => {
  if (listening) stopListening();
  else startListening();
});

$("#speak").addEventListener("click", () => {
  speakOn = !speakOn;
  $("#speak").classList.toggle("on", speakOn);
  if (speakOn) {
    // Unlock the audio element within this gesture so later async replies play.
    ttsAudio.play().then(() => ttsAudio.pause()).catch(() => {});
  }
});

// ── boot ────────────────────────────────────────────────────────────

if ("serviceWorker" in navigator) {
  navigator.serviceWorker.register("/sw.js").catch(() => {});
}

$("#composer").addEventListener("submit", (e) => {
  e.preventDefault();
  const input = $("#msg");
  const v = input.value.trim();
  if (!v) return;
  input.value = "";
  send(v);
});
$("#refresh").addEventListener("click", loadFleet);
$("#enable-push").addEventListener("click", enablePush);

// ── live feed (SSE) ─────────────────────────────────────────────────

function toast(title, body) {
  const el = document.createElement("div");
  el.className = "msg yscr";
  el.textContent = "🔔 " + title + " — " + body;
  $("#log").append(el);
  el.scrollIntoView({ block: "end" });
}

// Background activity: the server summarizing sessions in the background. Track
// the set of in-flight sessions and show a subtle strip while any are active.
const bgActive = new Map(); // session key → title
function renderBgActivity() {
  const el = $("#bg-activity");
  if (!bgActive.size) {
    el.hidden = true;
    el.textContent = "";
    return;
  }
  el.hidden = false;
  el.innerHTML = `<span class="spin"></span>Summarizing ${escape([...bgActive.values()].join(", "))}…`;
}

function connectStream() {
  if (!("EventSource" in window)) return false;
  const es = new EventSource("/api/stream");
  es.addEventListener("fleet", loadFleet);
  es.addEventListener("notice", (e) => {
    try {
      const n = JSON.parse(e.data);
      toast(n.title, n.body);
    } catch (_) {}
    loadFleet();
  });
  es.addEventListener("activity", (e) => {
    try {
      const a = JSON.parse(e.data);
      if (a.kind === "summarizing") bgActive.set(a.session, a.title || a.session);
      else bgActive.delete(a.session);
      renderBgActivity();
    } catch (_) {}
  });
  es.onerror = () => {}; // EventSource auto-reconnects
  return true;
}

loadFleet();
loadAudioConfig();
if (!connectStream()) {
  setInterval(loadFleet, 15000); // fallback poll where SSE is unavailable
}
