// YSCR service worker: an offline app shell + background push notifications.
//
// Strategy: NETWORK-FIRST for the shell. A previous cache-first version pinned
// installed clients to a stale shell (new UI never appeared until sw.js itself
// changed, and even then the cache was never busted) — a fresh install had to
// clear all site data to see updates. Network-first means an online load always
// gets fresh HTML/JS/CSS and refreshes the cache; the cache is only an offline
// fallback. /api is never cached. Bump CACHE on a breaking shell change so the
// activate handler evicts stale entries.

const CACHE = "yscr-v5";
const SHELL = ["/", "/index.html", "/app.js", "/pcm-worklet.js", "/styles.css", "/manifest.webmanifest", "/icon.svg"];

self.addEventListener("install", (e) => {
  e.waitUntil(caches.open(CACHE).then((c) => c.addAll(SHELL)).then(() => self.skipWaiting()));
});

self.addEventListener("activate", (e) => {
  e.waitUntil(
    caches.keys().then((keys) => Promise.all(keys.filter((k) => k !== CACHE).map((k) => caches.delete(k)))).then(() => self.clients.claim())
  );
});

// Network-first for the shell; cache is the offline fallback. /api always live.
self.addEventListener("fetch", (e) => {
  const url = new URL(e.request.url);
  if (url.pathname.startsWith("/api/")) return; // let it hit the network
  if (e.request.method !== "GET") return;
  e.respondWith(
    fetch(e.request)
      .then((res) => {
        // Refresh the cached copy of same-origin OK responses for offline use.
        if (res && res.ok && url.origin === self.location.origin) {
          const copy = res.clone();
          caches.open(CACHE).then((c) => c.put(e.request, copy)).catch(() => {});
        }
        return res;
      })
      .catch(() => caches.match(e.request).then((hit) => hit || caches.match("/index.html")))
  );
});

// Background push → a notification. Payload: {title, body, tag?, url?}.
self.addEventListener("push", (e) => {
  let data = { title: "YSCR", body: "" };
  try { data = e.data.json(); } catch (_) { data.body = e.data ? e.data.text() : ""; }
  e.waitUntil(
    self.registration.showNotification(data.title || "YSCR", {
      body: data.body || "",
      tag: data.tag || "yscr",
      icon: "/icon.svg",
      badge: "/icon.svg",
      data: { url: data.url || "/" },
      renotify: true,
    })
  );
});

// Focus (or open) the app when a notification is clicked.
self.addEventListener("notificationclick", (e) => {
  e.notification.close();
  const target = (e.notification.data && e.notification.data.url) || "/";
  e.waitUntil(
    self.clients.matchAll({ type: "window", includeUncontrolled: true }).then((cls) => {
      for (const c of cls) {
        if ("focus" in c) { c.navigate(target); return c.focus(); }
      }
      return self.clients.openWindow(target);
    })
  );
});
