# carvilon

Local-first property-management / intercom platform (Go).

This is a **monorepo** that holds the two code bases that are always built and
deployed together. They used to live in two separate repositories linked locally
via `go.work`; they are merged here into one repo with **both git histories
preserved** (grafted with `git subtree`, no squash).

## Layout

```
carvilon-server/    Platform core. A multi-module Go workspace:
                      server/          carvilon.local/server  (edge + cloud, main binary)
                      shared/          carvilon.local/shared  (wire format / proto)
                      mock/            carvilon.local/mock    (UDM device emulation)
                      license-server/  carvilon.local/license-server
streaming-server/   WebRTC media transport (WHIP/WHEP/TURN, pion).
                      Single module carvilon.local/stream.
go.work             Workspace file tying all of the above modules together.
```

## How the two relate

`streaming-server` is a *dumb media layer* (cameras and profiles, no tenants /
users / auth). All authorization lives in `carvilon-server`. The streaming code
is compiled **in-process** into the edge binary only under the `carvilon_stream`
build tag; the public build never imports `carvilon.local/stream` (verified via
`go list -deps`). The license boundary runs along the module boundary, not the
process boundary.

## Build

```powershell
# linux/arm64 edge build, commercial (in-process stream) variant
$env:GOOS="linux"; $env:GOARCH="arm64"
go build -tags carvilon_stream -ldflags="-s -w" -trimpath `
  -o bin\carvilon-server-linux-arm64 .\carvilon-server\server\cmd\carvilon-server
Remove-Item Env:GOOS; Remove-Item Env:GOARCH
```

Public build: drop `-tags carvilon_stream`.

## Licensing (intended split)

- `carvilon-server/` — platform core, AGPL.
- `streaming-server/` — MIT.

Each module is the authority for its own terms; see the license / NOTICE files
inside each subdirectory. (`carvilon-server/NOTICE.md` lists third-party
notices.)
