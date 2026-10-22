// Package static embeds static CSS, JavaScript, icon, and offline fallback assets into the Go binary.
package static

import "embed"

// FS holds embedded static assets (/static/*).
//
//go:embed *.css *.js *.svg *.png *.html
var FS embed.FS

// ServiceWorkerJS holds the root-scoped service worker script served at /sw.js.
//
//go:embed sw.js
var ServiceWorkerJS []byte
