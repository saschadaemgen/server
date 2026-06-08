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
- `cmd/cloudca` - mTLS mini-CA generator for the edge/cloud
  side-channel (stdlib only)

## Tech

- Go (stdlib-first; `net/http` + Go 1.22 ServeMux, no router lib)
- `modernc.org/sqlite` (pure Go, no CGO)
- Admin UI: Go templates + inline Lucide icons, via `go:embed`
- One binary, two roles chosen at runtime by a `-role` flag:
  cross-compiled `linux/arm64` for the per-site Raspberry Pi (edge)
  and `linux/amd64` for the cloud VPS
- `systemd` deployment (a user service on the edge, a system
  service on the cloud)

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
server (go2rtc has been removed) and, in the commercial build, is
linked in-process via the `carvilon_stream` build tag. Web viewer,
ESP monitor and the Android app authenticate against the same
`/webviewer` surface (session cookie for the browser, bearer token
for devices).

The cloud tier is no longer just planned - its foundation is built:
an edge/cloud split (one binary, a `-role` flag), an outbound mTLS
side-channel from the edge to the cloud, FCM push sent from the
edge, and in-process stream integration (commercial, the
`carvilon_stream` tag). The remote media path (ICE/STUN/TURN) is the
next step. The core stays local-first: doorbell and video work on
the LAN without any internet, and the cloud is strictly additive.

## Confidentiality

The core is intended for AGPL release. Until that release the
repository is private. The streaming-server and license-server
modules remain closed commercial components and are never part of
a public build.
