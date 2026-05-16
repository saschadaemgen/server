# unifix · Frontend Library

Production-ready, framework-free HTML/CSS/JS for the unifix tenant
intercom viewer and the Hausverwalter admin panel.

## Files

```
tokens.css                 CSS custom properties (dark + light themes)
components.css             All component classes — consume tokens only
interactions.js            Vanilla JS for sheets, modals, theme, SSE

intercom-idle.html         Tenant — idle screen (hosts the four
                           slide-up mode-layers: screensaver,
                           livestream, settings, history)
intercom-ringing.html      Tenant — Klingelt overlay (overlay)

admin-login.html           Hausverwalter — login card
admin-dashboard.html       Hausverwalter — overview + stat cards + activity
admin-users.html           Hausverwalter — Mieter table with Magic-Link state
admin-mocks.html           Hausverwalter — mock-event trigger + log
admin-settings.html        Hausverwalter — org / UA / Magic-Link / SMTP

modal-magic-link.html      Modal — shows generated Magic Link + QR

demo.html                  Single-page browser to preview all snippets
README.md                  This file
```

All HTML snippets are **body content only** — no `<!DOCTYPE>`, `<html>`,
`<head>` or `<body>` boilerplate. Drop them into your Go templates as
partials. Each snippet's required server data is documented in a comment
at the top.

## Integration

### Tenant routes

```html
<!DOCTYPE html>
<html lang="de">
<head>
  <link rel="stylesheet" href="/static/tokens.css">
  <link rel="stylesheet" href="/static/components.css">
</head>
<body class="stage-room">
  <div class="stage">
    {{template "intercom-idle"    .}}
    {{template "intercom-ringing" .}}    {{/* hidden until is-open */}}
  </div>
  <script src="/static/interactions.js"></script>
  <script>
    unifix.connectSSE("/intercom/{{.Token}}/events", {
      onDoorbellStart:  () => unifix.openOverlay("ringing"),
      onDoorbellCancel: () => unifix.closeOverlay("ringing"),
      onHistoryUpdate:  () => location.reload(),
    });
  </script>
</body>
</html>
```

### Admin routes

```html
<!DOCTYPE html>
<html lang="de" data-theme="dark">
<head>
  <link rel="stylesheet" href="/static/tokens.css">
  <link rel="stylesheet" href="/static/components.css">
</head>
<body class="admin-shell">
  {{template "admin-dashboard" .}}
  {{template "modal-magic-link" .}}    {{/* hidden until is-open */}}
  <script src="/static/interactions.js"></script>
</body>
</html>
```

## Theming

Themes are switched by setting `data-theme` on `<html>`:

```html
<html data-theme="dark">    <!-- default -->
<html data-theme="light">
```

`interactions.js` persists the user's choice in `localStorage`
(`unifix.theme`). It also handles `"system"` by following the OS
`prefers-color-scheme` and re-applying on change.

JS API:

```js
unifix.setTheme("dark" | "light" | "system");
unifix.cycleTheme();          // dark → light → system → dark
unifix.openSheet("history");
unifix.closeSheet("history");
unifix.openOverlay("ringing");
unifix.closeOverlay("ringing");
unifix.openModal("magic-link");
unifix.closeModal("magic-link");
unifix.setDND(true);
unifix.connectSSE(url, handlers);
```

## Declarative attributes

Most interactions are wired via `data-action=` attributes — no JS
needed in your templates:

| Attribute                                | Effect                                      |
| ---------------------------------------- | ------------------------------------------- |
| `data-action="open-history"`             | Opens the `data-sheet="history"` sheet      |
| `data-action="close-sheet"`              | Closes the nearest `[data-sheet]`           |
| `data-action="open-modal"` + `data-modal-name="x"` | Opens `[data-modal="x"]`          |
| `data-action="close-modal"`              | Closes the nearest `[data-modal]`           |
| `data-action="open-overlay"` + `data-overlay-name="x"` | Opens `[data-overlay="x"]`     |
| `data-action="ignore-call"`              | Closes the ringing overlay                  |
| `data-action="toggle-theme"`             | Cycles through themes                       |
| `data-action="toggle-dnd"`               | Flips DND indicator (UI only)               |
| `data-action="copy-magic-link"`          | Copies the modal's URL to clipboard         |

Esc closes any open modal → sheet → overlay (in that priority).

## Server-Sent Events contract

```
event: doorbell_start
data:  { "door": "Hauseingang", "ts": "2026-05-13T23:36:14Z" }

event: doorbell_cancel
data:  { "door": "Hauseingang", "ts": "2026-05-13T23:36:31Z" }

event: history_update
data:  { "items": [...] }
```

Wire them up with `unifix.connectSSE(url, handlers)` — see the
tenant-routes example above.

## Browser support

- Modern Chrome / Safari / Firefox / Edge (Evergreen).
- Uses `color-mix()` (Baseline 2023) for accent button tints.
- Uses `backdrop-filter` for frosted sheets / modals.
- Uses `EventSource` for SSE.
- No build step. No polyfills shipped.

## Previewing locally

Open `demo.html` via a static server (the demo loads admin snippets
via `fetch`, which fails on `file://`):

```sh
cd library
python3 -m http.server 8000
# open http://localhost:8000/demo.html
```
