// Package web embeds the yscr PWA (app shell + service worker + manifest).
package web

import "embed"

//go:embed index.html app.js sw.js styles.css manifest.webmanifest icon.svg
var FS embed.FS
