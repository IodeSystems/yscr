// YSCR service worker: an offline app shell + background push notifications.

const CACHE = "yscr-v1";
const SHELL = ["/", "/index.html", "/app.js", "/styles.css", "/manifest.webmanifest", "/icon.svg"];

self.addEventListener("install", (e) => {
  e.waitUntil(caches.open(CACHE).then((c) => c.addAll(SHELL)).then(() => self.skipWaiting()));
});

self.addEventListener("activate", (e) => {
  e.waitUntil(
    caches.keys().then((keys) => Promise.all(keys.filter((k) => k !== CACHE).map((k) => caches.delete(k)))).then(() => self.clients.claim())
  );
});

// Cache-first for the shell; never cache /api (always live).
self.addEventListener("fetch", (e) => {
  const url = new URL(e.request.url);
  if (url.pathname.startsWith("/api/")) return; // let it hit the network
  e.respondWith(
    caches.match(e.request).then((hit) => hit || fetch(e.request).catch(() => caches.match("/index.html")))
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
