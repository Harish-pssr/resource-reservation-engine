// Package web holds the console: static files served by the API, embedded so one binary is the whole product.
package web

import "embed"

// FS contains index.html, app.css and app.js.
//
//go:embed index.html app.css app.js
var FS embed.FS
