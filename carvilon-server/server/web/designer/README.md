# CARVILON Logic Editor — `web/designer/`

The CARVILON visual logic editor, shipped as a **single self-contained
`index.html`** (the full editor: project dropdown, 111-block palette + favorites,
module cards, sagging cyan wires, align/distribute toolbar, minimap, log dock).
No build step, no `dist/`, no framework toolchain — the file runs by double-click
and is served verbatim by the carvilon server.

> The earlier Vite + Svelte Flow scaffold (`src/`, `package.json`,
> `vite.config.mjs`) was the slim variant and has been **retired** in favour of
> this full version (decision confirmed by CD). It remains recoverable from git
> history and from the retired tree at
> `carvilon-server/server/design/web/designer/`.

## Layout

```
index.html            the full editor — one file, self-contained
vendor/               locally vendored assets (local-first; see below)
  lucide.min.js       Lucide icons, pinned 0.460.0
  fonts.css           @font-face for Inter + JetBrains Mono (rewritten to ./fonts/)
  fonts/*.woff2       the webfont subsets
embed.go              //go:embed index.html vendor  → designer.FS
```

## Local-first (no external requests)

The editor must work inside the building with no internet. Every asset the page
references is vendored under `vendor/` and the HTML points at relative paths:

- Lucide → `./vendor/lucide.min.js` (was `unpkg.com/lucide@0.460.0/...`).
- Fonts → `./vendor/fonts.css` (was the Google Fonts `css2` stylesheet + the two
  `preconnect` hints, which are removed). `fonts.css` `@font-face` `src` URLs were
  rewritten from `fonts.gstatic.com/...` to `./fonts/<file>.woff2`.

Loading the page issues **zero** requests to any external host. To re-vendor or
bump a version, download the resource, drop it under `vendor/`, and rewrite the
reference to a relative path — do not reintroduce a CDN URL.

## How it is served

`embed.go` bakes `index.html` + `vendor/` into the binary via `go:embed`. The
http surface serves it under `/a/designer/` behind the admin session gate, and
the admin page `/a/designer` embeds it in a full-bleed `<iframe>` for clean
isolation from the admin shell (see `internal/httpserver/handler_admin_designer.go`
and `templates/admin/designer.html`).

- `GET /a/designer`  → host page (admin chrome + iframe)
- `GET /a/designer/` → the editor bundle (`index.html` is the directory index)

## Out of scope (later tickets)

The log dock shows **demo/placeholder feeds** (SSH/MQTT/System/Engine). Real
engine/SSE feeds, persistence, and the editor → server graph binding are
separate tickets. The graph stays the hardcoded demo for now. (The status bar's
host label is real — fetched from `GET /a/designer/host`.)
