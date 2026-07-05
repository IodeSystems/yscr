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

loadFleet();
setInterval(loadFleet, 15000);
