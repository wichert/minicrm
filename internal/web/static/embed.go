// Package static embeds static CSS and JavaScript assets into the Go binary.
package static

import "embed"

// FS holds embedded static assets (/static/*).
//
//go:embed *.css *.js *.svg
var FS embed.FS
