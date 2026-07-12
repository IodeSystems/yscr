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
  stopSpeaking(); // a new turn cuts any reply still playing
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
    // Don't start TTS while the user is actively speaking a new utterance —
    // it would play over their voice. speak() re-checks after its async fetch.
    if (speakOn && reply && !userSpeaking()) speak(reply);
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
    renderQuestions(sessions);
    box.innerHTML = "";
    if (!sessions || !sessions.length) {
      box.innerHTML = '<div class="empty">Nothing active across any source.</div>';
      return;
    }
    for (const s of sessions) {
      const card = document.createElement("div");
      card.className = "card";
      const pend = (s.Pending || []).length;
      const pending = pend
        ? `<span class="pending" title="${pend} decision(s) awaiting you">▲${pend}</span>`
        : "";
      // Compact card in a horizontal scroller: dot + title on top, clamped
      // summary below. Fixed row height on mobile; swipe sideways for more.
      card.innerHTML = `
        <div class="top">
          <span class="dot ${s.Status}" title="${escape(s.Status)}"></span>
          <span class="title">${escape(s.Ref.Title || s.Ref.ID)}</span>
          ${pending}
        </div>
        <div class="summary">${escape(s.Summary || "")}</div>`;
      box.append(card);
    }
  } catch (e) {
    box.innerHTML = `<div class="empty">fleet unavailable (${e.message})</div>`;
  }
}

// ── questions awaiting the user ─────────────────────────────────────
// Visual half of the concierge's question handling: any session with a pending
// Questionnaire (e.g. a Claude CLI on an AskUserQuestion) is shown here with
// its options as tappable chips. A single-choice question answers on one tap;
// multi-select / multi-field questions toggle then Submit. The concierge can
// also answer conversationally — both drive the same source Actor.
function renderQuestions(sessions) {
  const box = $("#questions");
  const pend = [];
  for (const s of sessions || []) for (const q of s.Pending || []) pend.push({ s, q });
  if (!pend.length) {
    box.hidden = true;
    box.innerHTML = "";
    return;
  }
  box.hidden = false;
  box.innerHTML = "";
  for (const { s, q } of pend) box.append(questionCard(s, q));
}

function questionCard(s, q) {
  const card = document.createElement("div");
  card.className = "qcard";
  const answerable = q.Fields.some((f) => (f.Options || []).length);
  const oneTap = q.Fields.length === 1 && q.Fields[0].Type === "choice" && (q.Fields[0].Options || []).length > 0;
  const picks = {}; // field.Key → value (choice) or Set (multi)

  const head = document.createElement("div");
  head.className = "qhead";
  head.textContent = `${s.Ref.Source} · ${s.Ref.Title || s.Ref.ID}`;
  card.append(head);

  const qtext = document.createElement("div");
  qtext.className = "qtext";
  qtext.textContent = q.Intro || (q.Fields[0] && q.Fields[0].Prompt) || "Awaiting your answer";
  card.append(qtext);

  for (const f of q.Fields) {
    if (q.Fields.length > 1) {
      const fl = document.createElement("div");
      fl.className = "qfield";
      fl.textContent = f.Prompt;
      card.append(fl);
    }
    // No options (e.g. a multi-question tab prompt we can't drive from a card):
    // show the question read-only with guidance, no chips.
    if (!(f.Options || []).length) {
      const note = document.createElement("div");
      note.className = "qnote";
      note.textContent = f.Help || "Answer this in the terminal or ask the concierge.";
      card.append(note);
      continue;
    }
    const multi = f.Type === "multi";
    if (multi) picks[f.Key] = new Set();
    const opts = document.createElement("div");
    opts.className = "qopts";
    for (const o of f.Options || []) {
      const chip = document.createElement("button");
      chip.className = "chip";
      chip.textContent = o.Label || o.Value;
      if (o.Detail) chip.title = o.Detail;
      chip.addEventListener("click", () => {
        if (oneTap) return submitAnswer(card, s, q, { [f.Key]: o.Value });
        if (multi) {
          const set = picks[f.Key];
          set.has(o.Value) ? set.delete(o.Value) : set.add(o.Value);
          chip.classList.toggle("on");
        } else {
          picks[f.Key] = o.Value;
          opts.querySelectorAll(".chip").forEach((c) => c.classList.remove("on"));
          chip.classList.add("on");
        }
      });
      opts.append(chip);
    }
    card.append(opts);
  }

  if (!oneTap && answerable) {
    const submit = document.createElement("button");
    submit.className = "qsubmit";
    submit.textContent = "Submit answer";
    submit.addEventListener("click", () => {
      const answers = {};
      for (const f of q.Fields) {
        if (f.Type === "multi") answers[f.Key] = [...picks[f.Key]];
        else if (picks[f.Key] !== undefined) answers[f.Key] = picks[f.Key];
      }
      submitAnswer(card, s, q, answers);
    });
    card.append(submit);
  }
  return card;
}

async function submitAnswer(card, s, q, answers) {
  card.classList.add("busy");
  try {
    const r = await fetch("/api/answer", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ source: s.Ref.Source, id: s.Ref.ID, questionnaire_id: q.ID, answers }),
    });
    const data = await r.json().catch(() => ({}));
    if (!r.ok) throw new Error(data.error || r.status);
    card.className = "qcard done";
    card.innerHTML = `<div class="qdone">✓ answered</div>`;
    loadFleet();
  } catch (e) {
    card.classList.remove("busy");
    let err = card.querySelector(".qerr");
    if (!err) {
      err = document.createElement("div");
      err.className = "qerr";
      card.append(err);
    }
    err.textContent = "couldn't submit: " + e.message;
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

// userSpeaking: the user is mid-utterance in a hands-free session. Used to
// suppress starting TTS so a reply never plays over the user's own voice.
function userSpeaking() { return listening && hadSpeech; }

// VAD tunables: RMS energy to count as voice, trailing silence (ms) that ends an
// utterance, the minimum voiced span to send (drops clicks), and — for barge-in
// — a HIGHER threshold + a run of frames of loud input over the TTS to cut it.
// silenceMs is generous on purpose: people pause between phrases, and cutting a
// sentence off early is far more annoying than a beat of lag before it sends.
const VAD = { threshold: 0.012, silenceMs: 2600, minSpeechMs: 250, bargeThreshold: 0.06, bargeFrames: 5 };
let bargeCount = 0; // consecutive loud frames while TTS plays → barge-in

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
    // echoCancellation lets the browser subtract the played TTS from the mic so
    // barge-in (talking over a reply) works without headphones; the others clean
    // the input for the VAD + Whisper.
    micStream = await navigator.mediaDevices.getUserMedia({
      audio: { echoCancellation: true, noiseSuppression: true, autoGainControl: true },
    });
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

    // Barge-in: while the reply is playing, capture is paused (echo). Watch for a
    // sustained run of LOUD input (above residual echo) and cut the TTS — the
    // resumed segment then captures the rest of what you're saying.
    if (speaking) {
      if (rms > VAD.bargeThreshold) {
        if (++bargeCount >= VAD.bargeFrames) { bargeCount = 0; stopSpeaking(); }
      } else {
        bargeCount = 0;
      }
      vadRAF = requestAnimationFrame(tick);
      return;
    }
    bargeCount = 0;

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

let ttsUrl = null; // object URL of the reply currently loaded/playing

async function speak(text) {
  try {
    const r = await api("/api/audio/speech", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ input: text, model: audioCfg.tts_model, voice: audioCfg.tts_voice || undefined }),
    });
    const buf = await r.blob();
    if (userSpeaking()) return; // user started talking during the fetch — don't play over them
    stopSpeaking(); // cut any prior reply before starting this one
    ttsUrl = URL.createObjectURL(buf);
    ttsAudio.onended = () => stopSpeaking();
    ttsAudio.onerror = () => stopSpeaking();
    ttsAudio.src = ttsUrl;
    setSpeaking(true); // suppress VAD capture + show the interruptible status
    ttsAudio.play().catch((e) => { console.warn("tts play failed", e); stopSpeaking(); });
  } catch (e) {
    console.warn("tts fetch failed", e);
    stopSpeaking();
  }
}

// stopSpeaking interrupts the current reply — from a manual tap, a voice barge-in,
// or a new message — and hands control back to listening.
function stopSpeaking() {
  if (!speaking && !ttsUrl) return;
  try { ttsAudio.pause(); ttsAudio.currentTime = 0; } catch (_) {}
  if (ttsUrl) { URL.revokeObjectURL(ttsUrl); ttsUrl = null; }
  setSpeaking(false);
}

// setSpeaking gates hands-free capture during TTS: pause the live segment
// recorder so the concierge's own voice isn't recorded, and resume (fresh
// silence window) when playback ends. It also drives the interruptible status.
function setSpeaking(v) {
  speaking = v;
  if (segRec) {
    if (v && segRec.state === "recording") segRec.pause();
    if (!v && segRec.state === "paused") { hadSpeech = false; silenceStart = 0; segRec.resume(); }
  }
  if (v) setStatus("Speaking… tap to stop", "think");
  else idleStatus();
}

// ── voice settings (persisted per browser) ──────────────────────────
// Sensitivity slider (6..36) ↔ RMS threshold: higher slider = lower threshold =
// picks up quieter/trailing speech.
const sensToThreshold = (s) => (42 - s) / 1000;
const thresholdToSens = (t) => Math.round(42 - t * 1000);

function loadVadSettings() {
  const sm = parseInt(localStorage.getItem("yscr.silenceMs") || "", 10);
  if (sm >= 800 && sm <= 6000) VAD.silenceMs = sm;
  const th = parseFloat(localStorage.getItem("yscr.threshold") || "");
  if (th >= 0.006 && th <= 0.036) VAD.threshold = th;
}

function initSettings() {
  const sr = $("#silence-range"), so = $("#silence-out");
  sr.value = VAD.silenceMs;
  const showS = () => (so.textContent = (VAD.silenceMs / 1000).toFixed(1) + "s");
  showS();
  sr.addEventListener("input", () => {
    VAD.silenceMs = parseInt(sr.value, 10);
    showS();
    localStorage.setItem("yscr.silenceMs", String(VAD.silenceMs));
  });

  const nr = $("#sens-range"), no = $("#sens-out");
  nr.value = thresholdToSens(VAD.threshold);
  const showN = () => (no.textContent = nr.value);
  showN();
  nr.addEventListener("input", () => {
    VAD.threshold = sensToThreshold(parseInt(nr.value, 10));
    showN();
    localStorage.setItem("yscr.threshold", String(VAD.threshold));
  });

  $("#settings-btn").addEventListener("click", () => {
    const p = $("#settings");
    p.hidden = !p.hidden;
  });
}

// Tap to toggle hands-free listening: tap on and it listens continuously,
// auto-sending each utterance on a trailing pause (VAD); tap off to stop.
// Either way, first cut any reply that's currently playing.
const mic = $("#mic");
mic.addEventListener("click", () => {
  stopSpeaking();
  if (listening) stopListening();
  else startListening();
});

// Tap the status line while a reply is playing to stop it (manual interrupt).
$("#status").addEventListener("click", () => { if (speaking) stopSpeaking(); });

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
  // Reload once when a new service worker takes over (a fresh deploy) so an
  // open PWA runs the new JS instead of the stale build it loaded with. Skip the
  // first-ever install (no prior controller) so there's no startup reload.
  const hadController = !!navigator.serviceWorker.controller;
  navigator.serviceWorker.register("/sw.js").catch(() => {});
  navigator.serviceWorker.addEventListener("controllerchange", () => {
    if (hadController) location.reload();
  });
}

loadVadSettings(); // apply saved per-user VAD tuning before any listening
initSettings();

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
