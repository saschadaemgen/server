# streaming-server

CARVILON streaming-server. Go-Library plus eine spike-Binary.

- Module-Pfad: `carvilon.local/stream`
- Repo-Name: `streaming-server` (Pfad und Name weichen bewusst voneinander ab)

## Saison

- **S1 (durch):** Multi-Source-Architektur, UniFi Protect-Quelle (RTSPS,
  eigener RFC-6184-Depacketizer), Live-Bild auf der Testseite.
- **S2 (durch):** Fan-Out — EINE Kamera, N WebRTC-Viewer. Erster Viewer
  triggert den Kamera-Pull; letzter Viewer beendet ihn. Drop-statt-buffer
  pro Subscriber.
- **S3 (durch):** MJPEG-Output für ESP/Browser via ffmpeg-Subprozess.
  Byte-Schnitt 1:1 wie go2rtc — Drop-in für den carvilon-Proxy.
- **S4 (durch):** Multi-Kamera, bedarfsgesteuert. Pro
  `(CameraID, Quality)` ein eigener Lifecycle. Alle Viewer-Endpoints
  sind profile-driven (`?src=<name>`); Profile binden Kamera, Quality
  und Usage (browser / esp). **Eine Kamera wird NUR gepullt, wenn
  mindestens ein Viewer sie gerade anschaut** — sonst kein RTSP-Pull,
  kein Decode, kein ffmpeg. 5 Kameras, 0 Viewer = 0 Last.
- **S5 (durch):** Profile-Persistenz in SQLite (`modernc.org/sqlite`,
  pure-Go, kein cgo). DB ist Source of Truth nach erstem Start; JSON-
  Seed nur in leere DB. Naht-Interface `streambackend.Backend` zur
  carvilon-Seite; build-tag-Wrapper (`carvilon_stream`) verdrahtet die
  beiden Repos ohne Public-Build-Pollution.
- **S6 (durch, ESP-Mess-Apparat):** Profile bekommen Codec + Encode-
  Parameter (Width/Height/FPS/EncodeQuality). `/stream/stats` JSON
  liefert per-Client + global frames/bytes/avg-fps/avg-kbps; Linux-
  `/proc`-CPU als globaler Vergleichswert. PUT/DELETE
  `/api/profiles/{name}` für Live-Tuning ohne Restart. Drei MJPEG-
  Profile (`mjpeg_hq` / `mjpeg_bal` / `mjpeg_fast`) plus die H.264-
  CBP-Transcode-Variante (`h264_cbp` über `/stream/h264`, Annex-B
  ohne AUDs, SPS/PPS vor jedem IDR, eine AU pro HTTP-Chunk, GoP=1s)
  ab Werk. Profil-#101 ist eine DB-Row, kein Code-Change.
- **Schritt 7+** (GenericRTSPSource, Audio, ESP32-Quelle) sind explizit
  **nicht** Teil dieser Stufe.

## Voraussetzungen

- Go ≥ 1.25 (gortsplib v5 Anforderung; das Repo testet mit 1.26.1).
- LAN-Zugriff zum UDM (Port 443/TCP für die Protect-API, 7441/TCP für die RTSPS-Quelle).
- Ein Browser auf demselben LAN für den Empfang.
- Einen UniFi-Integration-API-Key mit Camera-/Stream-Scope.

## Konfiguration

API-Key, Host, Camera-ID gehören **nicht ins Repo.** Sie werden nur lokal
zur Laufzeit gesetzt.

```sh
cp .env.example .env
# .env editieren — niemals committen (durch .gitignore abgedeckt)
```

| Env-Variable               | Pflicht | Default               | Bedeutung                                                                                                          |
| -------------------------- | ------- | --------------------- | ------------------------------------------------------------------------------------------------------------------ |
| `UNIFI_NVR_HOST`           | ja      | —                     | Host des UDM, z.B. `192.168.1.1`                                                                                   |
| `UNIFI_API_KEY`            | ja      | —                     | Protect-Integration-Key (Settings → Integrations)                                                                  |
| `UNIFI_CAMERA_ID`          | nein¹   | Intercom-Default-Cam  | Welche Kamera fürs S6-Default-Set; bei leer nimmt der Spike die hart kodierte Intercom-CameraID                    |
| `CARVILON_PROFILES_JSON`   | nein¹   | —                     | JSON-Liste fürs Multi-Kamera-Setup (siehe `.env.example`); überschreibt sowohl `UNIFI_CAMERA_ID` als auch das Default-Set |
| `CARVILON_STREAM_DB`       | nein    | `./state/stream.db`   | Pfad zur SQLite-Profil-DB. **Hat die DB einmal Profile, gewinnt sie über JSON** (S5-Regel).                         |
| `CARVILON_STREAM_BASE_URL` | nein    | `http://127.0.0.1`+Listen | Absolute Basis-URL für `MJPEGURL` / `WebRTCSignalURL` im Backend (carvilon-Proxy ruft das auf)                  |
| `UNIFI_ENCRYPTION`         | nein    | `tls`                 | Wire-Protection: `tls` (heute) oder `srtp` (zukünftig, heute Fehler — siehe unten)                                 |
| `CARVILON_STREAM_LISTEN`   | nein    | `:8555`               | HTTP-Listen-Adresse (Signaling + Testseite)                                                                        |
| `CARVILON_FFMPEG`          | nein    | `ffmpeg`              | Pfad zum ffmpeg-Binary für die MJPEG-Pipeline (Standard via `$PATH`)                                               |
| `CARVILON_DISABLE_MJPEG`   | nein    | —                     | Nicht-leerer Wert deaktiviert MJPEG (WebRTC-only-Runs ohne ffmpeg)                                                 |

¹ Seed-Priorität bei leerer DB:
1. `CARVILON_PROFILES_JSON` gesetzt → JSON wird geseedet.
2. `UNIFI_CAMERA_ID` gesetzt → S6-Mess-Set auf dieser Kamera.
3. Keins von beidem → S6-Mess-Set auf der eingebauten Intercom-Default-CameraID (**S6-03**, nur im Spike).

Hat die DB schon Profile, gewinnt sie unabhängig vom Env immer (S5-Regel).
Das eingebaute Default-Set existiert **nur in `cmd/spike`** — die `streambackend`-Naht
(Produktiv-Pfad ueber carvilon-server) startet leer; der carvilon-Admin füllt sie via CRUD.

Ports `9080` (carvilon-server) und `1984` (go2rtc) werden bewusst gemieden.

## Starten (Windows / PowerShell)

```powershell
$env:UNIFI_NVR_HOST = '192.168.1.1'
$env:UNIFI_API_KEY  = '<protect-integration-key>'
go run .\cmd\spike
```

## Starten (Linux / macOS)

```sh
export UNIFI_NVR_HOST='192.168.1.1'
export UNIFI_API_KEY='<protect-integration-key>'
go run ./cmd/spike
```

Zero-Config-Start (S6-03): ohne `UNIFI_CAMERA_ID` und ohne
`CARVILON_PROFILES_JSON` seedet der Spike automatisch das eingebaute
S6-Mess-Default-Set (5 Profile auf der hard-coded Intercom-CameraID).
Genau dafür gedacht: `git pull && go run .\cmd\spike` und sofort
durchmessen. Die DB wird zur Wahrheit, ab dem zweiten Start ist das
Default-Set egal — Tuning ueber `PUT /api/profiles/{name}`.

Danach im Browser öffnen:

```
http://<host>:8555/
```

→ Profil aus dem Dropdown wählen, **Connect** klicken. Sobald der
ICE-State `connected` ist und der erste IDR durch den Depacketizer
geflossen ist, zeigt das `<video>`-Element das Live-Bild der gewählten
Kamera (typisch 1–5 s nach Connect, abhängig vom GoP-Intervall).

**Multi-Kamera testen** (mit `CARVILON_PROFILES_JSON` konfiguriert):

- Tab 1 mit Profil A → Server-Log: nur Kamera A wird gepullt.
- Tab 2 mit Profil B → zusätzlich Kamera B startet.
- Tab 1 schließen → Kamera A stoppt, Kamera B läuft weiter.
- Beide Tabs schließen → beide Kameras stoppen.
- Profil C wurde nie aufgerufen → Kamera C wurde NIE gepullt.

**Fan-Out auf einer Kamera testen:** dieselbe Profil-URL in mehreren
Browser-Tabs öffnen. Server-Log zeigt genau **EIN** `unifi: got RTSPS URL`
und **EIN** `unifi: first IDR`, egal wie viele Viewer.

**MJPEG testen:** Browser direkt auf
`http://<host>:8555/api/stream.mjpeg?src=mjpeg_bal` (oder ein anderes
MJPEG-Profil aus dem Default-Set). Format byte-identisch zu go2rtc.

**Profile-Liste auslesen:** `GET /api/profiles` liefert JSON mit allen
registrierten Profilen (Name, CameraID, Quality, Usage, Description,
Codec, Width, Height, FPS, EncodeQuality) — die Testseite nutzt das,
um das Dropdown zu füllen.

## HTTP-Endpoints (S6-Stand)

| Methode + Pfad                       | Zweck                                                                                   |
| ------------------------------------ | --------------------------------------------------------------------------------------- |
| `POST /offer?src=<name>`             | WebRTC-Offer (nur Profile mit `codec=h264_passthrough`)                                 |
| `GET  /api/stream.mjpeg?src=<name>`  | MJPEG-Stream (nur Profile mit `codec=mjpeg`)                                            |
| `GET  /stream/h264?src=<name>`       | H.264 Constrained Baseline Annex-B fuer ESP (nur Profile mit `codec=h264_cbp`) — eine AU pro HTTP-Chunk, SPS/PPS vor jedem Keyframe |
| `GET  /api/profiles`                 | Liste aller Profile als JSON (mit Codec + Encode-Parametern)                            |
| `PUT  /api/profiles/{name}`          | Profil anlegen / tunen — Body wie `.env.example` Beispiel, Validate vor Persistenz      |
| `DELETE /api/profiles/{name}`        | Profil entfernen (laufende Viewer bleiben auf altem Hub bis Disconnect)                 |
| `GET  /stream/stats`                 | JSON-Snapshot: per-Client + per-Profil + global (frames, bytes, avg-fps, avg-kbps, cpu) |
| `GET  /healthz`                      | `204 No Content` (Liveness)                                                             |

### S6 Mess-Workflow

1. **Default-Set kommt von alleine.** Zero-Config-Start
   (`UNIFI_NVR_HOST` + `UNIFI_API_KEY` reichen) seedet fünf Profile auf
   der eingebauten Intercom-Default-CameraID: `intercom_web`,
   `mjpeg_hq`, `mjpeg_bal`, `mjpeg_fast`, `h264_cbp` (letzteres ist nur
   in der DB; der Endpoint kommt erst, wenn das tinyH264-Input-
   Format aus dem ESP-Chat klar ist). Mit `UNIFI_CAMERA_ID` läuft das
   Set auf einer anderen Kamera; mit `CARVILON_PROFILES_JSON` ein
   vollständig eigenes Set.
2. **Viewer öffnen.** ESP / `curl` / Browser zieht ein MJPEG-Profil,
   z.B. `curl -o /dev/null http://host:8555/api/stream.mjpeg?src=mjpeg_bal`.
3. **Messen.** `curl http://host:8555/stream/stats | jq`. Pro Client
   sieht man `frames_sent`, `bytes_sent`, `avg_fps`, `avg_bitrate_kbps`;
   global zusätzlich `transcoder_cpu_percent` (nur Linux).
4. **Tunen.** Encode-Parameter via `PUT /api/profiles/{name}` ändern:
   ```sh
   curl -X PUT http://host:8555/api/profiles/mjpeg_bal \
        -H 'Content-Type: application/json' \
        -d '{"camera_id":"<cam>","quality":"high","usage":"esp","codec":"mjpeg","width":800,"height":1280,"fps":10,"encode_quality":5}'
   ```
   Neue Viewer erleben sofort die neuen Werte (alte Viewer bleiben
   auf ihrer Session, bis sie disconnecten). Periodisches Stats-Log
   im stderr (alle 30 s) zeigt den Effekt.
5. **Profil hinzufügen.** Statt `mjpeg_bal` zu überschreiben kann
   man auch ein neues Profil per PUT anlegen — der Server kennt es
   sofort, ohne Restart. Profil-#101 ist eine DB-Row, kein Code-
   Change.

### H.264-CBP gegenüber MJPEG einmessen (S6-02)

Der `h264_cbp`-Pfad transcodiert die Kamera-H.264 zu Constrained
Baseline und liefert sie als chunked Annex-B an `/stream/h264?src=
h264_cbp`. Wire-Shape ist im Briefing festgenagelt:

- Annex-B mit 4-Byte-Startcodes; KEINE AUDs.
- SPS + PPS vor jedem IDR (in-band, repeat headers ueber `-bsf:v
  dump_extra=freq=keyframe`).
- Eine komplette Access Unit pro HTTP-Chunk. Keyframe-AU = SPS + PPS
  + IDR im SELBEN Chunk; P-Frame-AU = einzelner Non-IDR-Slice.
- GoP = 1 Sekunde, laufzeit-justierbar per `PUT /api/profiles/h264_cbp`.

Server-seitig gegen `ffprobe` verifizieren (der echte ESP-Decode-Test
kommt vom ESP-Chat, sobald deren Decode-Pfad steht):

```sh
# Profil + Stream-Inhalt
curl -s http://host:8555/stream/h264?src=h264_cbp \
  | ffprobe -hide_banner -i - 2>&1 \
  | grep -E "profile|codec_name|frame_rate"
# erwartet: codec_name=h264, profile=Constrained Baseline, fps=15
```

Telemetrie:
- `transcoder_cpu_percent` in `/stream/stats` (Linux) zeigt den CPU-
  Preis des Transcodes.
- Pro `/stream/h264`-Client tauchen `frames_sent`, `bytes_sent`,
  `avg_fps`, `avg_bitrate_kbps` im Snapshot auf — 1:1 vergleichbar
  mit den MJPEG-Profilen, was den Sinn des Experiments ausmacht.

## Cross-Compile für Raspberry Pi (arm64)

```sh
GOOS=linux GOARCH=arm64 go build -o bin/spike ./cmd/spike
```

## Architektur

```
Stream-Kern (carvilon.local/stream)
   kennt nur das VideoSource-Interface
        │
        ▼
   source.VideoSource (internal/source)
        │
        ├── UniFiProtectSource           ← jetzt
        │       Protect-API → RTSPS-URL
        │       gortsplib v5 (RTP-Pull)
        │       internal/h264 (eigener Depacketizer)
        │       AU-Assembly + SPS/PPS-Prepend
        │
        ├── GenericRTSPSource            ← später
        ├── ESP32Source                  ← später
        └── (weitere)
```

Im Spike (heute, mit Fan-Out):

```
UA-Intercom (RTSPS:7441)
   │  gortsplib v5: TLS, Describe, Setup, OnPacketRTP
   ▼
internal/h264.Depacketizer
   │  alle sechs RFC-6184-Packetization-Typen
   │  (FU-A/B, STAP-A/B, MTAP-16/24, Single NAL)
   ▼
UniFiProtectSource (AU-Assembly per Marker/Timestamp)
   │  Frames-Channel
   ▼
internal/hub.Hub  ─────────────────────────────────  (Fan-Out, S2-01)
   │  - genau EIN Source-Pull, egal wie viele Viewer
   │  - Source-Lifecycle: Start@1st-Subscriber, Stop@last
   │  - letztes IDR gecached fuer Pre-Feed an neue Subscriber
   │  - drop-statt-buffer pro Subscriber (1x/s gedrosseltes Log)
   │
   ├──────────────┬──────────────┬──────────────┬───  ...
   ▼              ▼              ▼              ▼
Subscriber 1   Subscriber 2   Subscriber 3   Subscriber N
(eigene PC,    (eigene PC,    (eigene PC,    (eigene PC,
 eigener        eigener        eigener        eigener
 Track)         Track)         Track)         Track)
   │              │              │              │
   ▼              ▼              ▼              ▼
pion TrackLocalStaticSample.WriteSample (pro Viewer)
   │  Annex-B + Duration aus PTS-Delta
   │  Packetisierung + DTLS-SRTP raus zum Browser
   ▼
Browser-Tab    Browser-Tab    Browser-Tab    Browser-Tab
```

Ein langsamer Subscriber kann **niemals** Source oder andere Viewer
ausbremsen — der Bus verteilt non-blocking pro Subscriber-Channel.
Backpressure endet beim einzelnen Subscriber-Buffer (Default 30 AUs,
≈1 s bei 30 fps).

### Package-Layout

```
streaming-server/
├── cmd/spike/         (Binary; baut Profile + Source-Factory + Stats)
├── server.go          (HTTP /offer + /api/stream.mjpeg + /api/profiles + /stream/stats)
├── streambackend/     (carvilon-Naht: Backend, Profile/Camera-Wireshape, INTEGRATION.md)
├── web/index.html     (Testseite mit Profil-Dropdown)
├── internal/
│   ├── h264/          (RFC-6184-Depacketizer + Unit-Tests)
│   ├── h264esp/       (H.264-CBP-Transcode + Annex-B-AU-Splitter + Fan-Out-Hub, S6-02)
│   ├── hub/           (H.264-Fan-Out-Bus + Source-Lifecycle + IDR-Cache)
│   ├── mjpeg/         (ffmpeg-Encoder + JPEG-Fan-Out + go2rtc-Multipart)
│   ├── profile/       (Profile-Struktur + Registry, S4-01 + S6 Codec/Encode-Felder)
│   ├── sourcereg/     (Source-Registry pro (CameraID,Quality), S4-01)
│   ├── store/         (SQLite-Profile-Persistenz mit additiver Migration, S5)
│   ├── unifiapi/      (Protect-Integration-Client: ListCameras, S5)
│   ├── stats/         (per-Client throughput-Counter, atomic, S6-01)
│   ├── proccpu/       (linux: /proc/<pid>/stat-Sampler, S6-01)
│   ├── droplog/       (rate-limited drop-counter)
│   └── source/
│       ├── source.go  (VideoSource-Interface)
│       └── unifi/     (UniFiProtectSource)
```

### Multi-Kamera-Lifecycle (S4)

```
Profile-Registry: {"intercom_browser" → {cam=A, q=high, usage=browser}, ...}
                                    │
                                    ▼ Lookup by ?src=
                          Source-Registry (sourcereg)
                                    │
                  ┌─────────────────┴─────────────────┐
                  │      lazy: Hub per (cam,q)        │
                  │                                   │
                  ▼                                   ▼
            hub.Hub für (A, high)             hub.Hub für (B, high)
                  │                                   │
        ┌─────────┴─────────┐                  ┌─────┴─────┐
        ▼                   ▼                  ▼           ▼
   WebRTC-Viewer         MJPEG-Session    WebRTC-Viewer  (idle wenn 0 Viewer)
   für Profil A          für Profil A
   (browser-usage)       (esp-usage)
```

**Bedarfsgesteuert**: hat eine `(CameraID, Quality)` keinen einzigen
Viewer (über alle Profile hinweg, browser + esp), wird die Quelle
nicht gepullt. Bei 5 Kameras und 0 Zuschauern: 0 RTSP-Pulls,
0 Decodes, 0 ffmpeg-Instanzen. Nur Hub-Bookkeeping (KB) bleibt im
Speicher.

### MJPEG-Pipeline (S3)

```
H.264-Hub  ──Subscriber──►  mjpeg-Session (per Profil)
                                    │
                            forwarder Goroutine
                                    │ AU → Annex-B → ffmpeg stdin
                                    ▼
                            ffmpeg-Subprozess
                                    │ decode → scale → JPEG-encode
                                    ▼ stdout (concat. JPEGs, FF D8 ... FF D9)
                            FrameSplitter (SOI/EOI scan)
                                    │
                                    ▼
                          mjpeg-Fan-Out → N HTTP-Clients
                                            │
                                            ▼
                                  multipart/x-mixed-replace
                                  (byte-exakt wie go2rtc)
```

Pro Profil ein eigener ffmpeg-Encoder. Wenn beide `intercom_esp` und
`intercom_browser` gleichzeitig aktiv sind, laufen zwei ffmpegs (jeder
mit eigenem Decode). Briefing-akzeptierter Trade-off — Optimierung
"ein Decode, zwei Encodes" via `-filter_complex` ist ein späteres
Briefing, kein Code-Strukturwechsel.

**ffmpeg-Voraussetzung:** ein installiertes `ffmpeg` im `$PATH` (auf
dem RPi via go2rtc ohnehin vorhanden). Startup prüft via
`ffmpeg -version` und bricht hart ab wenn fehlt. `CARVILON_DISABLE_MJPEG=1`
für reine WebRTC-Runs.

## Sicherheitsmodell

Die Kamera-zu-Server-Strecke läuft per **TLS** (rtsps:// auf Port 7441 der
UDM) — verschlüsselt zwischen den beiden kontrollierten LAN-Endpunkten.
Innerhalb des TLS-Tunnels wird **Plain-RTP** transportiert. Das
entspricht dem `rtspx://`-Pfad, den go2rtc seit Jahren für UniFi-Kameras
nutzt, und der etablierten CARVILON-Setup-Linie aus dem ESP-Projekt.

Konkret: die Protect-API liefert URLs mit `?enableSrtp`. Mit diesem
Query-Schalter aktiviert UniFi zusätzlich SRTP (RFC 3711) mit MIKEY-
Schlüsseltausch in der SDP. Diese Variante ist in der Go-Welt nicht
gelöst (go2rtc Issue #81 offen seit 2022), und der eingebaute MIKEY-
Decrypt-Pfad in gortsplib aktiviert sich nicht, weil UniFi die SDP als
`RTP/AVP` (nicht `RTP/SAVP`) auszeichnet — eine UniFi-Inkonsistenz.

`UniFiProtectSource` strippt im TLS-Modus daher den `?enableSrtp`-
Parameter, bevor die URL gortsplib erreicht. UniFi liefert dann Plain-
RTP im TLS-Tunnel — direkt dekodierbar durch unseren Depacketizer.
Siehe Commit `f1da18e` (SDP-Befund-Logging) und das S1-07-Bewertungs-
Briefing für die Argumentation.

Der ausgehende Weg zum Browser ist immer DTLS-SRTP (WebRTC), unabhängig
vom UniFi-Modus.

**`srtp`-Modus als Backlog-Eintrag.** Das `UNIFI_ENCRYPTION`-Schalter-
feld kennt `srtp` als zweiten Wert, der heute mit einem klaren
`ErrEncryptionSRTPNotImplemented` rausfliegt. Damit ist die
Konfigurations-Form stabil, falls eine MIKEY+SRTP-Implementierung
später nachzieht. Der dafür plausible Pfad (Weg B3 aus der S1-07-
Bewertung): gortsplibs public `pkg/mikey`-Parser auf `medi.KeyMgmtMikey`,
`pion/srtp/v3` als neue Top-Level-Dep, ein Wrapper, der die Pakete vor
unserem Depacketizer entschlüsselt. Aufwand ~3–5 Tage, kein praktischer
LAN-Mehrschutz gegenüber TLS — daher heute nicht gebaut.

## Bekannte Stolpersteine

- **TLS ohne IP-SAN.** Sowohl Protect-API als auch RTSPS laufen über die
  UDM mit Self-signed-Cert ohne IP-SAN. Aktuell `InsecureSkipVerify`
  beidseits. Später: gegen die UDM-CA pinnen wie der carvilon-UA-Client.
  Diese Härtung ist der real wirksame Sicherheits-Hebel und nicht eine
  zweite Verschlüsselungs-Schicht.
- **UA-Intercom-Packetization.** Die Kamera deklariert in der SDP
  `packetization-mode=1`, sendet aber das volle Mode-2-Spektrum
  (FU-B / STAP-B / MTAP-16 / MTAP-24). gortsplib v5 lehnt diese ab — der
  eigene Depacketizer (`internal/h264`) behandelt sie. STAP-A mit Non-
  Zero-Padding nach Zero-Size-Marker (auch eine UA-Eigenheit) wird
  tolerant zu Ende gelesen statt verworfen.
- **Port 7447 ist tot** auf dieser UDM-SE (in ESP-Saison 1 verifiziert).
  Der UniFi-Pfad geht ausschließlich über `rtsps://` auf 7441; ein
  Fallback auf unverschlüsselte RTSP-Verbindungen ist explizit nicht
  vorgesehen.
- **Drop statt Buffer.** Der Frames-Channel zwischen Quelle und Kern
  hat ein winziges Buffer (4) und non-blocking Send. Wenn der Consumer
  hinterherhinkt, wird gedroppt. Das Drop-Logging ist gedrosselt
  (1× pro Sekunde mit Summe), damit echte Anomalien sichtbar bleiben.
- **Sicherheit / Geheimnisse.** API-Key, Host, Camera-ID nur per Env-Var.
  Niemals ins Repo. Der API-Key und die fertige RTSPS-URL (Token!)
  werden niemals geloggt — die SDP-Befund-Ausgabe redaktiert Inline-Keys
  und MIKEY-Payloads explizit (mit Unit-Tests, `internal/source/unifi/sdp_test.go`).

## Dependency-Doktrin

Top-Level-Abhängigkeiten sind ausschließlich:

- `github.com/bluenviron/gortsplib/v5` (RTSP-Transport + SDP)
- `github.com/pion/webrtc/v4` (WebRTC, packetization, SRTP)
- `github.com/pion/rtp` (transitiv über pion, brauchen wir direkt für RTP-Packet-Typen)
- `modernc.org/sqlite` (pure-Go SQLite — S5-freigegeben, kein cgo)

Weitere Dritt-Libs vorher mit dem Stream-Chat klären. Der Depacketizer ist
bewusst in-tree (`internal/h264`) gebaut, um die Doktrin sauber zu halten —
und um unabhängig zu sein davon, was eine Fremd-Lib zufällig kann.
Genauso bewusst: kein gopsutil für die CPU-Sample — `internal/proccpu`
liest `/proc/<pid>/stat` direkt; nicht-Linux liefert einen Stub.

## Tests

Vier Test-Suites, alle stdlib-`testing`, keine zusätzlichen Deps:

```sh
go test ./...
go test -race ./internal/hub/...   # zusätzlich: race-detector
```

| Paket | Was getestet wird | Zweck |
| --- | --- | --- |
| `internal/h264` | RFC-6184-Depacketizer (18 Tests, alle sechs Packetization-Typen, Edge-Cases wie Seq-Gap, Start+End in einem Paket, STAP-A-Padding-Toleranz) | Wenn das Live-Bild fehlt, aber diese Tests grün sind, liegt der Fehler in Quelle/Verdrahtung — nicht im Depacketizer. |
| `internal/hub` | H.264-Fan-Out-Bus (17 Tests, alle Lifecycle-Pfade, Slow-Subscriber-Isolation, IDR-Pre-Feed, Source-Restart, Concurrency-Stress mit `-race`) | Beweist die Properties, die go2rtc bei UniFi nie ganz hingekriegt hat: ein Pull für viele, drop-statt-buffer, kein Source-Block durch einen langsamen Viewer. |
| `internal/mjpeg` multipart + splitter | go2rtc-byte-kompatibler Multipart-Writer + JPEG-SOI/EOI-Splitter (13 Tests, byte-exact, Edge-Cases wie Pre-SOI-Banner) | Lockt den Wire-Schnitt fest — der carvilon-Proxy forwarded verbatim. |
| `internal/mjpeg` profiles + encoder | Strukturiertes Profil-Modell + ffmpeg-Args-Construction (8 Tests, alle Felder validate, Args-Layout korrekt, CheckFFmpeg-Fehlerpfad) | Profile sind editierbar ohne Code-Change, ffmpeg-Misconfig schlägt fail-fast. |
| `internal/mjpeg` hub | JPEG-Fan-Out (14 Tests, Per-Profil-Lifecycle, Single-Encoder-für-N, Crash-Kaskade, race-clean) | EIN ffmpeg pro Profil, egal wie viele Clients. |
| `internal/source/unifi` SDP-Tests | `sdpSecurityReport` redaktiert Inline-Keys und MIKEY-Payloads | Geheimnisse dürfen NIE im Log landen — Tests prüfen das aktiv (5 Tests). |
| `internal/source/unifi` Encryption-Tests | `stripEnableSrtp` und `NewSource`-Encryption-Validierung | Sicherstellt: `enableSrtp` weg, andere Query-Felder bleiben, `srtp`-Modus kommt mit klarem Fehler raus (10 Tests). |

Insgesamt 111 Tests, alle grün. Diese Trennschärfe ist Absicht — bei
einem Live-Problem zeigen die Tests, ob das Problem im Code oder in
der Verdrahtung liegt.

S4-spezifisch:

| Paket | Tests | Was beweisen die |
| --- | --- | --- |
| `internal/profile` | 12 | Profile validate, Registry lookup, ByUsage-Filter, Duplikat-Erkennung |
| `internal/sourcereg` | 12 | Lazy-Hub, 0 Pull bei 0 Viewer, 2-Kameras unabhaengiger Lifecycle, shared-Pull bei selber Key, race-clean |

S5/S6-spezifisch:

| Paket | Tests | Was beweisen die |
| --- | --- | --- |
| `internal/store` | viele | CRUD-Round-Trip, Persistence über Open/Close, **Migration idempotent und backfillt Pre-S6-Rows** (codec='' → korrekter Codec via Usage), SeedIfEmpty respektiert DB-Wahrheit |
| `streambackend` | viele | URL-Builder + Query-Escape, CRUD inkl. Codec-Round-Trip, Sentinels (`ErrProfileNotFound`/`ErrNotConfigured`), JSON-Wire-Tags gelockt (carvilon-Wrapper kopiert feldweise) |
| `internal/profile` (S6) | + | Codec validate (passthrough ignoriert Encode-Params, mjpeg/h264_cbp verlangen sie), Range-Checks pro transcodiertem Codec |
| `internal/stats` (S6) | 12 | Register/Unregister, atomic-Counter, JSON-Tag-Lock, -race smoke (8 goroutines × 1000 Frames) |
| `internal/proccpu` (S6) | 4 | Konstruktor-Vertrag, first-call-not-ok-Regel, Stub-Verhalten auf Nicht-Linux |
| `package stream` (S6) | 12 | `/stream/stats` empty-snapshot, PUT/DELETE `/api/profiles/{name}` happy path + 400 / 404 / 503, Logger-goroutine respektiert ctx |
| `internal/h264esp` (S6-02) | viele | **AU-Splitter** — Keyframe-AU = SPS+PPS+IDR coalesced, P-Frame-AU einzeln, AUDs gestripped, 3-Byte-Startcodes akzeptiert/Output immer 4-Byte, EOF-Flush ohne Header-only-AU, slow-Reader byteweise. **ffmpeg-Args** — `-profile:v baseline -bf 0 -refs 1 -g <fps> -keyint_min <fps> -sc_threshold 0 -bsf:v dump_extra=freq=keyframe -f h264` gelockt; `aud=insert` verboten. **Hub** — ein Transcode für N Clients, Drop-Strategie, Lifecycle (bedarfsgesteuert), Encoder-Ende schliesst alle Subscribers, Concurrency-Stress mit `-race`. |
| `package stream` (S6-02) | 6 | `/stream/h264` — 503 ohne ffmpeg, 400/404 für falsche src, 405 für POST, Codec-Gate (Resolver) |
