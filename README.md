# carvilon-server

Go monorepo for the carvilon platform - a third-party companion
layer for UniFi Access hardware in multi-tenant residential
buildings. It adds a tenant- and device-facing layer (web viewer,
ESP indoor monitor, Android app) on top of UniFi Access, with its
own streaming server and doorbell-call lifecycle.

## Model

Open-core, in the spirit of Matrix/Element:

- **carvilon-server** (this repo) - the core, AGPL planned.
- **streaming-server** + **license-server** - commercial, closed,
  linked only in the commercial build via the `carvilon_stream`
  build tag. The public build never imports them.

## Modules

- `server` - main product server: REST API, SSE, admin UI, web
  viewer, viewer-goroutine manager, persistence
- `mock` - UA Intercom Viewer emulator (the embedded goroutines
  the server registers with the UDM)
- `shared/proto` - UniFi wire-format helpers (discovery, MQTT,
  RPC, TLV, WebSocket)
- `cmd/license-server` - commercial license validation (closed)

## Tech

- Go (stdlib-first; `net/http` + Go 1.22 ServeMux, no router lib)
- `modernc.org/sqlite` (pure Go, no CGO)
- Admin UI: Go templates + Tailwind (CDN) + htmx + Lucide, via
  `go:embed`
- Cross-compiled `linux/arm64`, runs on a Raspberry Pi per site

## Build

```
build\build-linux-arm64.ps1
```

## Deploy

```
build\deploy-rpi.ps1
```

## Status

Active development. The streaming layer runs on the platform's own
server (go2rtc has been removed). Web viewer, ESP monitor and the
Android app authenticate against the same `/webviewer` surface
(session cookie for the browser, bearer token for devices). A
cloud tier (edge/cloud split, remote access, push) is planned.

## Confidentiality

The core is intended for AGPL release. Until that release the
repository is private. The streaming-server and license-server
modules remain closed commercial components and are never part of
a public build.
