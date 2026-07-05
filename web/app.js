// YSCR PWA client. Talks to the yscr service; registers a service worker for
// background + push notifications.

const $ = (s) => document.querySelector(s);
const api = (p, opts) => fetch(p, opts).then((r) => (r.ok ? r : Promise.reject(new Error(r.status))));

// ── conversation ────────────────────────────────────────────────────

function bubble(text, cls) {
  const el = document.createElement("div");
  el.className = "msg " + cls;
  el.textContent = text;
  $("#log").append(el);
  el.scrollIntoView({ block: "end" });
  return el;
}

async function send(message) {
  bubble(message, "you");
  const pending = bubble("…", "yscr");
  try {
    const r = await api("/api/converse", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ message }),
    });
    const { reply } = await r.json();
    pending.textContent = reply || "(no reply)";
    if (speakOn && reply) speak(reply);
  } catch (e) {
    pending.className = "msg err";
    pending.textContent = "error: " + e.message;
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
let recorder = null;
let chunks = [];

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

async function startRecording() {
  if (recorder) return;
  let stream;
  try {
    stream = await navigator.mediaDevices.getUserMedia({ audio: true });
  } catch (_) {
    return;
  }
  chunks = [];
  recorder = new MediaRecorder(stream);
  recorder.ondataavailable = (e) => e.data.size && chunks.push(e.data);
  recorder.onstop = async () => {
    stream.getTracks().forEach((t) => t.stop());
    const blob = new Blob(chunks, { type: recorder.mimeType || "audio/webm" });
    recorder = null;
    await transcribeAndSend(blob);
  };
  $("#mic").classList.add("on");
  recorder.start();
}

function stopRecording() {
  if (recorder && recorder.state !== "inactive") recorder.stop();
  $("#mic").classList.remove("on");
}

async function transcribeAndSend(blob) {
  const fd = new FormData();
  fd.append("file", blob, "speech.webm");
  if (audioCfg.stt_model) fd.append("model", audioCfg.stt_model);
  try {
    const r = await api("/api/audio/transcriptions", { method: "POST", body: fd });
    const data = await r.json();
    const text = (data.text || "").trim();
    if (text) send(text);
  } catch (e) {
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
    const audio = new Audio(URL.createObjectURL(buf));
    audio.play().catch(() => {});
  } catch (_) {}
}

// Hold-to-talk (pointer) — press to record, release to transcribe + send.
const mic = $("#mic");
mic.addEventListener("pointerdown", (e) => { e.preventDefault(); startRecording(); });
mic.addEventListener("pointerup", (e) => { e.preventDefault(); stopRecording(); });
mic.addEventListener("pointerleave", stopRecording);
mic.addEventListener("pointercancel", stopRecording);

$("#speak").addEventListener("click", () => {
  speakOn = !speakOn;
  $("#speak").classList.toggle("on", speakOn);
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
  es.onerror = () => {}; // EventSource auto-reconnects
  return true;
}

loadFleet();
loadAudioConfig();
if (!connectStream()) {
  setInterval(loadFleet, 15000); // fallback poll where SSE is unavailable
}
