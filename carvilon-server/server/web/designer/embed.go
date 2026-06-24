// Package designer embeds the CARVILON visual logic editor and exposes
// it as an embed.FS for the http surface to serve.
//
// The editor is a single self-contained index.html (the full editor:
// project dropdown, 111-block palette, module cards, log dock) plus a
// locally vendored asset tree under vendor/ (Lucide icons pinned to
// 0.460.0, and the Inter + JetBrains Mono webfonts). Everything is
// baked into the binary and served under /a/designer/ behind the admin
// session gate. Local-first: the page makes no network request to any
// external host (no unpkg, no Google Fonts) when it loads.
//
// The log dock currently shows demo/placeholder feeds; wiring the real
// engine/SSE feeds is a later ticket.
package designer

import "embed"

// FS holds index.html and the vendor/ tree (vendor/lucide.min.js,
// vendor/fonts.css, vendor/fonts/*.woff2). It is served verbatim by the
// httpserver designer handler — no build step, no dist/.
//
//go:embed index.html vendor
var FS embed.FS
