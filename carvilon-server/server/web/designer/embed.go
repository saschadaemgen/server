// Package designer embeds the CARVILON visual logic editor and exposes
// it as an embed.FS for the http surface to serve.
//
// The editor is a thin index.html shell that loads css/editor.css and
// the ES-module entry js/main.js (the full editor: project dropdown,
// 111-block palette, module cards, log dock — split across focused
// modules under js/). Alongside sits a locally vendored asset tree
// under vendor/ (Lucide icons pinned to 0.460.0, and the Inter +
// JetBrains Mono webfonts). Everything is baked into the binary and
// served under /a/designer/ behind the admin session gate. There is no
// build step: the modules are served as static files. Local-first: the
// page makes no network request to any external host (no unpkg, no
// Google Fonts) when it loads.
//
// The log dock is real end to end: SSH terminals (xterm.js over a
// server-side bridge), MQTT broker client, System Log SSE and engine
// events. TCP/UDP are honest empty placeholders until terminal-track
// step 2.
package designer

import "embed"

// FS holds index.html, the css/ and js/ module trees, and the vendor/
// tree (vendor/lucide.min.js, vendor/fonts.css, vendor/fonts/*.woff2).
// It is served verbatim by the httpserver designer handler — no build
// step, no dist/.
//
//go:embed index.html css js vendor
var FS embed.FS
