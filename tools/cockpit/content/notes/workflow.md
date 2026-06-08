# Workflow & Konventionen

Die Prozessregeln für die Arbeit am CARVILON-Monorepo. Gerendert aus
`content/notes/workflow.md` — Pflege per Brief.

## Repo

- **Kanonisches Repo:** `C:\Projects\Carvilon\server` (Monorepo).
  `carvilon-server/` + `streaming-server/` als Module unter einem `go.work`.
- Die sibling-Ordner `C:\Projects\Carvilon\carvilon-server\` und
  `…\streaming-server\` sind **retired merge-sources** — nicht mehr bearbeiten.
- **Ein** Chat/Track (nur Android separat).

## Git-Disziplin

- **Lokal** auf `main` committen, **Conventional Commits**, logisch getrennt.
- **Kein Push** — Sascha pusht selbst.
- Getrackt wird nur die **Schale** des Cockpits (`tools/cockpit/`) + Manifest +
  `system-map.js`. Diese tragen **keine** echten IPs/MACs/Secrets.

## Sicherheit (hart)

- Living-Docs sind **gitignored** (echte Anlagen-Daten). Das Cockpit **liest**
  sie zur Laufzeit — **nie** in einen getrackten Ordner kopieren (sonst Drift +
  Leak).
- Diagramm-Labels **generisch** (RPi Edge, VPS Cloud) — keine echten Werte.
- **Pre-Push-Grep** über den getrackten Teil vor jedem Push — sucht nach den
  bekannten sensiblen Werten (VPS-Public-IP, LAN-Subnetz, Controller-/UDM-MAC,
  License-/Geräte-IDs). Die konkreten Patterns liegen **lokal** in der
  gitignored `tools/cockpit/scripts/secret-patterns.txt` (nie committen):
  `bash tools/cockpit/scripts/pre-push-grep.sh`
- Server bindet nur `127.0.0.1` und serviert aus Doc-Roots nur `*.md`.

## Build

```powershell
# linux/arm64 Edge-Build, kommerziell (in-process Stream)
$env:GOOS="linux"; $env:GOARCH="arm64"
go build -tags carvilon_stream -ldflags="-s -w" -trimpath `
  -o bin\carvilon-server-linux-arm64 .\carvilon-server\server\cmd\carvilon-server
Remove-Item Env:GOOS; Remove-Item Env:GOARCH
```

Public-Build: `-tags carvilon_stream` weglassen. Verifikation, dass der
Public-Build `carvilon.local/stream` **nicht** importiert: `go list -deps`.

## Cockpit pflegen

- Inhalts-Dateien per Brief je Season aktualisieren — die Seite zeigt es
  **sofort** (kein Rebuild). Das Aktualisieren gehört in die
  **Saison-End-Checkliste**.
- Schale starten: `go run ./tools/cockpit/serve.go` → http://127.0.0.1:7878
- Vendor-Libs neu erzeugen (selten): `cd tools/cockpit && npm ci && npm run vendor`.
