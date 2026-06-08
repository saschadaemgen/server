# Korrekturen & Stand — Saison 20

Kuratierte, **generische** Errata zu den Living-Docs (Stand 2026-06-08). Diese
Notiz korrigiert die Inkonsistenzen aus der Doku-Prüfung, **ohne** die
gitignored Living-Docs zu verändern — sie werden weiter zur Laufzeit so
gerendert, wie sie sind. Jede Korrektur unten ist gegen den **echten Code**
verifiziert (Datei/Stelle generisch benannt).

## 1 · Schema-Stand: 19 → **24**

Die Docs standen auf Schema 19; real ist **24**. Migrationen 020–024 nachtragen:

| Migration | Inhalt |
|---|---|
| 020 `viewer_doors` | 1:n Tür-Zuordnung je Viewer (Access-Door-UUIDs) |
| 021 `viewer_path_mode` | Transport-Weg-Override (auto / local / cloud) |
| 022 `viewer_setting_visibility` | Pro-Setting-Sichtbarkeit fürs Mieter-UI |
| 023 `viewer_resolution_mode` | Quell-Auflösung (high / medium / low) |
| 024 `viewer_cloud_stream_profile` | **zweites** Profil-Feld je Viewer (Cloud) |

## 2 · S19-Profil-Umbau: zwei Profil-Felder je Viewer

Bestätigt: je Viewer gibt es jetzt **`StreamProfile` (LAN)** und
**`CloudStreamProfile` (Cloud)**, beide Admin-wählbar (zwei Selects). Spalte
`cloud_stream_profile` via Migration 024.

## 3 · Offener Keying-Bug (Wurzel unverändert)

`cloud_stream_profile` **greift nicht** — es kommt immer `intercom_web`
(Live-Beweis: hohe Auflösung/Bitrate trotz gesetztem `intercom_med`).
Führende Hypothese: der **Cloud-WHIP-Publish ist auf die Klingel/Doorbell-ID
gekeyed**, nicht auf die Viewer-ID → `GetViewerInfo(streamID)` findet den
Viewer nicht → Default-Profil. Das per-Viewer-Cloud-Feld sitzt dann evtl. am
falschen Ort (gehört ggf. an die Klingel / den Publish-Key). Diskriminator:
Edge-Restart → es greift das Profil des ersten frischen Publish.

## 4 · Ein Binary (in-process), „merge deferred“ vereinheitlichen

Der Streaming-Layer läuft **in-process** im carvilon-Binary (Build-Tag
`carvilon_stream`) — Edge **und** Cloud sind dieselbe Quelle, dreifach gebaut.
Es gibt **kein** separates Stream-Binary mehr. Zwei Stellen
(stream-architecture §9, stream-feature-backlog) sagen noch „merge **deferred**“
— das meint nur den **Single-Repo-Quell-Merge (GOPRIVATE)**; das in-process
Open-Core-Embedding via Build-Tag ist **gebaut**. Wording angleichen:
*in-process embedding gebaut; nur der Quell-Repo-Merge bleibt deferred.*

## 5 · Ports (verifiziert)

- **:8443** Side-Channel — *carvilon Master* (`config.go` default).
- **:8444** WHIP-Ingress **+** WHEP-Egress (ein TLS-Mux, privates Cloud-CA-Cert).
- **:8446** optionaler **öffentlicher** WHEP-Egress (publik vertrautes Cert),
  S19-Opt-in, **zusätzlich** zu :8444 (das bleibt unangetastet).
- **3478** TURN/STUN (UDP), **5349** turns: (TLS). Reverse-Proxy-Front **:443**.
- **:8447 existiert nirgends** im Repo — die „signal :8447“-Notiz ist falsch;
  der signaling-nahe Port ist der interimistische Loopback-Hook (Code/Handover).

→ Die Briefing-Annahme „:8443 Side-Channel **vs** :8444 WHIP/WHEP als
Widerspruch“ ist keiner: es sind **verschiedene Module** (Master-Side-Channel
:8443 ↔ Stream-WHIP/WHEP :8444).

## 6 · go2rtc-Lizenz — **offene Frage, nicht stillschweigend ändern**

Das **Briefing S20** sagt: go2rtc ist **MIT** (nicht AGPL) → die Stream-Notes-
Regel „NEVER AGPL (go2rtc + mediamtx excluded)“ habe eine falsche Prämisse.
**Aber:** **jede** Doku im Repo behandelt go2rtc (und mediamtx) **als AGPL** und
begründet damit den Eigenbau (MIT). Das ist ein echter Widerspruch zwischen
Briefing-Annahme und Repo-Stand.

→ **Vor dem Ändern verifizieren** (echte Upstream-Lizenz von go2rtc und
mediamtx prüfen, Quelle festhalten). Wenn go2rtc real MIT ist, sind die
betroffenen Doku-Stellen zu korrigieren — die *Replace-go2rtc*-**Begründung**
(eigener MIT-Server) bleibt davon unberührt. mediamtx separat prüfen. Bis dahin
**keine** stille Änderung.

## 7 · Profil-Katalog — Code-Seed = **5**, „9 live“ unbelegt

Das Briefing erwartete „9 live, intercom_android/default/med/4g fehlen“. Im Code
ist der **Seed = 5**: `intercom_web`, `mjpeg_hq`, `mjpeg_bal`, `mjpeg_fast`,
`h264_cbp`. Die Namen `intercom_android` / `default` / `med` / `4g` existieren
**nicht** als Seed-Profile (sie leben in Tests/Konzept). „9 live“ konnte am Code
**nicht** belegt werden → Quelle prüfen (ggf. laufzeit-erweitert via
`CARVILON_PROFILES_JSON` oder DB, dann dort dokumentieren).

## 8 · Veraltetes Framing

`C:\Projects\UniFi…`-Pfade und „master chat / ESP chat / this track“ sind
überholt → jetzt **`C:\Projects\Carvilon\server`**, **ein** Repo, **ein** Chat
(nur Android separat).
