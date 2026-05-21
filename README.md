# streaming-server

CARVILON streaming-server. Go-Library plus eine spike-Binary.

- Module-Pfad: `carvilon.local/stream`
- Repo-Name: `streaming-server` (Pfad und Name weichen bewusst voneinander ab)

## Saison

- **S1, Schritt 1 (jetzt):** Multi-Source-Architektur + ein WebRTC-Viewer
  im Browser. UniFi Protect-Quelle (RTSPS, mit eigenem RFC-6184-
  Depacketizer) ist die erste konkrete Implementierung des `VideoSource`-
  Interfaces. Erfolg = Live-Bild der UA-Intercom auf der Testseite mit
  subjektiv niedriger Latenz, ohne Decoder-Fehler im Server-Log.
- **Schritt 2+** (Fan-Out, MJPEG-Output, GenericRTSPSource, ESP32-Quelle,
  Andocken an carvilon-server, Audio) sind explizit **nicht** Teil dieser
  Stufe.

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

| Env-Variable               | Pflicht | Default | Bedeutung                                                                              |
| -------------------------- | ------- | ------- | -------------------------------------------------------------------------------------- |
| `UNIFI_NVR_HOST`           | ja      | —       | Host des UDM, z.B. `192.168.1.1`                                                       |
| `UNIFI_API_KEY`            | ja      | —       | Protect-Integration-Key (Settings → Integrations)                                       |
| `UNIFI_CAMERA_ID`          | ja      | —       | Protect-Camera-ID                                                                       |
| `UNIFI_QUALITY`            | nein    | `high`  | Stream-Tier (`high` / `medium` / `low`)                                                 |
| `UNIFI_ENCRYPTION`         | nein    | `tls`   | Wire-Protection: `tls` (heute) oder `srtp` (zukünftig, heute Fehler — siehe unten)      |
| `CARVILON_STREAM_LISTEN`   | nein    | `:8555` | HTTP-Listen-Adresse (Signaling + Testseite)                                             |

Ports `9080` (carvilon-server) und `1984` (go2rtc) werden bewusst gemieden.

## Starten (Windows / PowerShell)

```powershell
$env:UNIFI_NVR_HOST   = '192.168.1.1'
$env:UNIFI_API_KEY    = '<protect-integration-key>'
$env:UNIFI_CAMERA_ID  = '<camera-id>'
go run .\cmd\spike
```

## Starten (Linux / macOS)

```sh
export UNIFI_NVR_HOST='192.168.1.1'
export UNIFI_API_KEY='<protect-integration-key>'
export UNIFI_CAMERA_ID='<camera-id>'
go run ./cmd/spike
```

Danach im Browser öffnen:

```
http://<host>:8555/
```

→ **Connect** klicken. Sobald der ICE-State `connected` ist und der erste
IDR durch den Depacketizer geflossen ist, sollte das `<video>`-Element das
Live-Bild der Intercom-Kamera zeigen (typisch 1–5 s nach Connect, abhängig
vom GoP-Intervall der Kamera).

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

Im Spike (heute):

```
UA-Intercom (RTSPS:7441)
   │  gortsplib v5: TLS, Describe, Setup, OnPacketRTP
   ▼
internal/h264.Depacketizer
   │  alle sechs RFC-6184-Packetization-Typen
   │  (FU-A/B, STAP-A/B, MTAP-16/24, Single NAL)
   ▼
UniFiProtectSource (AU-Assembly per Marker/Timestamp)
   │  Frames-Channel (drop-statt-buffer)
   ▼
stream.Server (consumer-loop)
   │  Annex-B-Marshal + Sample.Duration aus PTS-Delta
   ▼
pion TrackLocalStaticSample.WriteSample
   │  pion packetisiert + SRTP fuer den Browser
   ▼
Browser-Tab  →  <video>
```

### Package-Layout

```
streaming-server/
├── cmd/spike/         (Binary)
├── server.go          (Stream-Kern: HTTP-Signaling, sample-Writer)
├── web/index.html     (Testseite)
├── internal/
│   ├── h264/          (RFC-6184-Depacketizer + Unit-Tests)
│   ├── droplog/       (rate-limited drop-counter)
│   └── source/
│       ├── source.go  (VideoSource-Interface)
│       └── unifi/     (UniFiProtectSource)
```

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

Weitere Dritt-Libs vorher mit dem Stream-Chat klären. Der Depacketizer ist
bewusst in-tree (`internal/h264`) gebaut, um die Doktrin sauber zu halten —
und um unabhängig zu sein davon, was eine Fremd-Lib zufällig kann.

## Tests

Drei Test-Suites, alle stdlib-`testing`, keine zusätzlichen Deps:

```sh
go test ./...
```

| Paket | Was getestet wird | Zweck |
| --- | --- | --- |
| `internal/h264` | RFC-6184-Depacketizer (18 Tests, alle sechs Packetization-Typen, Edge-Cases wie Seq-Gap, Start+End in einem Paket, STAP-A-Padding-Toleranz) | Wenn das Live-Bild fehlt, aber diese Tests grün sind, liegt der Fehler in Quelle/Verdrahtung — nicht im Depacketizer. |
| `internal/source/unifi` SDP-Tests | `sdpSecurityReport` redaktiert Inline-Keys und MIKEY-Payloads | Geheimnisse dürfen NIE im Log landen — Tests prüfen das aktiv (5 Tests). |
| `internal/source/unifi` Encryption-Tests | `stripEnableSrtp` und `NewSource`-Encryption-Validierung | Sicherstellt: `enableSrtp` weg, andere Query-Felder bleiben, `srtp`-Modus kommt mit klarem Fehler raus (10 Tests). |

Insgesamt 33 Tests, alle grün. Diese Trennschärfe ist Absicht — bei
einem Live-Problem zeigen die Tests, ob das Problem im Code oder in
der Verdrahtung liegt.
