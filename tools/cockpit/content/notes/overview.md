# CARVILON Cockpit

Ein zentrales Planungs- und Doku-Cockpit für CARVILON — eine **served static
site**, gestartet über einen lokalen Mini-Server. Schale und Inhalt sind
getrennt: die **Schale** (React + React Flow + Markdown) ist statisch, der
**Inhalt** bleibt als Dateien im Repo und wird zur Laufzeit gerendert — eine
Quelle der Wahrheit, kein Drift, Updates ohne Rebuild.

## Tabs

- **Bauplan** — das datengetriebene System-Diagramm (`content/system-map.js`).
  Pan/Zoom/Fit, Minimap, animierte Kanten und **Szenario-Playback**
  (Klingel / Tür öffnen / Video-LAN / Video-Cloud) Schritt für Schritt durch
  die Blöcke.
- **Doku** — die gerenderten Living-Docs (Architektur / Security / Wire-Format /
  Decisions / Backlog) für carvilon-server **und** streaming-server.
- **Planung** — To-Dos + aktuelle Season-Aufgaben als Board.
- **Workflow** — die Prozessregeln / Konventionen.
- **Archiv** — Season-Protokolle + Übergaben, **beide** Doku-Sätze (carvilon +
  stream) + die Projekt-Saison-Dokumente, nach Season sortiert.

## System in einem Satz

CARVILON ist eine **local-first** Property-Management-/Intercom-Plattform (Go):
ein **RPi-Edge** (ein Binary, role=edge) mit der vollen lokalen Funktion —
arbeitet ohne Internet — und eine **VPS-Cloud** (role=cloud) als rein
**additiver** Fernzugriffs-Bridge. Ein Cloud-Ausfall blockiert die Edge nie.
Der Streaming-Layer ist **in-process** ins Edge-Binary kompiliert (Build-Tag
`carvilon_stream`); UniFi (Intercom + Access) trägt Klingel und Tür; Clients
sind Android-App, Browser-WebViewer und das ESP-Tür-Modul.

## Sicherheit

Die Living-Docs sind **gitignored** (echte Anlagen-Topologie). Das Cockpit
**liest** sie zur Laufzeit an ihren bestehenden Pfaden — sie werden **nie** in
den getrackten Baum kopiert. Getrackt sind nur die Schale, das Manifest und
`system-map.js`; diese tragen **keine** echten IPs/MACs/Secrets. Sichtbare
Diagramm-Labels sind generisch. Der Server bindet nur an `127.0.0.1` und
serviert aus den Doc-Roots **ausschließlich** `*.md`.
