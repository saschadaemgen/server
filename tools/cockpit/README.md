# CARVILON Cockpit

Ein zentrales Planungs- und Doku-Cockpit als **served static site**: System-
Bauplan, Living-Docs, To-Dos, Workflow und Archiv hinter Tab-Menüs, animiert.

## Starten (eine Zeile)

```powershell
go run ./tools/cockpit/serve.go
```

Dann **http://127.0.0.1:7878** öffnen. Der Server bindet nur an `localhost`
(die Living-Docs tragen echte Anlagen-Daten) und serviert aus den Doc-Roots
ausschließlich `*.md`.

> Nicht per Doppelklick / `file://` öffnen — der Browser blockiert dann das
> Laden der Inhalts-Dateien (CORS). Es muss über den Mini-Server laufen.

## Aufbau

```
tools/cockpit/
  serve.go              dependency-freier Go-Static-Server (stdlib) + Doc-API
  public/
    index.html          lädt vendor/lib.js + content/* + app/*
    styles.css
    app/                ESM-Module (kein Build): main, bauplan, doku, planung, workflow, archiv
    vendor/lib.js       gebündelte Libs (React, React Flow, dagre, marked, htm) — offline
    vendor/xyflow.css
  content/              GETRACKTE Inhalts-Daten (eine Quelle der Wahrheit)
    system-map.js       window.SYSTEM_MAP — das Bauplan-Modell (generisch!)
    manifest.js         Tabs -> Datei-Pfade
    todos.js            Planung-Board
    notes/*.md          kuratierte, generische Notizen (Übersicht, Korrekturen, Workflow)
  package.json          NUR zum Neu-Erzeugen von vendor/lib.js (nicht zum Betrieb nötig)
  scripts/build-vendor.mjs
```

## Schale vs. Inhalt

Die **Schale** (public/, vendor/) ist statisch und ändert sich selten. Der
**Inhalt** bleibt als Dateien im Repo und wird zur Laufzeit gerendert:

- **Bauplan** ← `content/system-map.js`
- **Doku / Workflow / Archiv** ← echte `*.md` (Living-Docs an ihren bestehenden,
  gitignored Pfaden; gelesen über `/docs/<root>/…`)
- **Planung** ← `content/todos.js`

**Inhalt pflegen = Datei ändern + Seite neu laden. Kein Rebuild.**

## Doc-Roots

`serve.go` löst benannte, read-only Roots auf, indem es Kandidaten-Pfade prüft
(erster Treffer gewinnt) — robust gegenüber der Tatsache, dass die gitignored
carvilon-Living-Docs nach dem Merge nicht unter `server/` liegen:

| Root | sucht |
|---|---|
| `carvilon-docs` | `server/carvilon-server/docs` → `../carvilon-server/docs` |
| `carvilon-seasons` | `server/carvilon-server/seasons` → `../carvilon-server/seasons` |
| `stream-docs` | `server/streaming-server/docs` |
| `stream-seasons` | `server/streaming-server/seasons` |
| `project-docs` | `../docs` |

Welche Roots gefunden wurden, steht beim Start im Server-Log und unter
`/api/roots`.

## Vendor-Libs neu erzeugen (selten)

```powershell
cd tools/cockpit
npm ci
npm run vendor    # -> public/vendor/lib.js (committed)
```

`node_modules/` ist gitignored; `public/vendor/lib.js` ist committed und das
Einzige, was der Betrieb braucht.

## Sicherheit

- Living-Docs **nie** in den getrackten Baum kopieren — nur zur Laufzeit lesen.
- Getrackt (Schale, Manifest, system-map.js) trägt **keine** echten IPs/MACs/
  Secrets; Diagramm-Labels generisch.
- Vor Push: Pre-Push-Grep über den getrackten Teil (siehe Workflow-Tab).
