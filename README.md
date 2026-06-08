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
build tag; a build without that tag does not link `carvilon.local/stream`. That
build-tag seam is an architectural boundary (retained as a separate decision),
not a licensing one - the whole product is proprietary (see License below).

## Build

```powershell
# linux/arm64 edge build, commercial (in-process stream) variant
$env:GOOS="linux"; $env:GOARCH="arm64"
go build -tags carvilon_stream -ldflags="-s -w" -trimpath `
  -o bin\carvilon-server-linux-arm64 .\carvilon-server\server\cmd\carvilon-server
Remove-Item Env:GOOS; Remove-Item Env:GOARCH
```

Plain build (without the in-process stream): drop `-tags carvilon_stream`.

## License

Copyright (c) 2026 Sascha Daemgen IT and More Systems.
All rights reserved. Proprietary and confidential.

CARVILON is a commercial, proprietary product. No part of this repository is
released under an open-source license, and there is no public source release.
(`carvilon-server/NOTICE.md` documents the third-party components the product
bundles or remote-loads, with their respective upstream licenses.)
