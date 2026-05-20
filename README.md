# streaming-server

CARVILON streaming-server. Go-Library plus eine spike-Binary.

- Module-Pfad: `carvilon.local/stream`
- Repo-Name: `streaming-server` (Pfad und Name weichen bewusst voneinander ab)

## Saison

- **S1, Schritt 1 (jetzt):** Machbarkeits-Spike — RTSP-Pull (UA-Intercom) + ein
  WebRTC-Viewer im Browser. Single-Viewer, kein Fan-Out, kein Transcode, kein
  Audio. Erfolg = Live-Bild auf der Testseite mit subjektiv niedriger Latenz.
- **Schritt 2+** (Fan-Out, MJPEG-Output, VideoSource-Interface, Andocken an
  carvilon-server, Audio) sind explizit **nicht** Teil dieser Stufe.

## Voraussetzungen

- Go ≥ 1.25 (gortsplib v5 Anforderung; das Repo testet mit 1.26.1).
- LAN-Zugriff zur UniFi Intercom (Port 7441/TCP).
- Ein Browser auf demselben LAN für den Empfang.

## Konfiguration

Die RTSPS-URL enthält ein eingebettetes Token. **Sie gehört nicht ins Repo.**
Sie wird nur lokal zur Laufzeit gesetzt.

```sh
cp .env.example .env
# .env editieren — niemals committen (durch .gitignore abgedeckt)
```

| Env-Variable                     | Pflicht | Default   | Bedeutung                                              |
| -------------------------------- | ------- | --------- | ------------------------------------------------------ |
| `CARVILON_STREAM_RTSP_SOURCE`    | ja      | —         | `rtsps://HOST:7441/<feed-id>?enableSrtp`               |
| `CARVILON_STREAM_LISTEN`         | nein    | `:8555`   | HTTP-Listen-Adresse (Signaling + Testseite)            |

Ports `9080` (carvilon-server) und `1984` (go2rtc) werden bewusst gemieden.

## Starten (Windows / PowerShell)

```powershell
$env:CARVILON_STREAM_RTSP_SOURCE = 'rtsps://192.168.1.1:7441/<id>?enableSrtp'
go run .\cmd\spike
```

## Starten (Linux / macOS)

```sh
export CARVILON_STREAM_RTSP_SOURCE='rtsps://192.168.1.1:7441/<id>?enableSrtp'
go run ./cmd/spike
```

Danach im Browser öffnen:

```
http://<host>:8555/
```

→ **Connect** klicken. Sobald der ICE-State `connected` ist, sollte das
`<video>`-Element das Live-Bild der Intercom-Kamera zeigen.

## Cross-Compile für Raspberry Pi (arm64)

```sh
GOOS=linux GOARCH=arm64 go build -o bin/spike ./cmd/spike
```

## Architektur (Spike-Scope)

```
UA-Intercom (RTSPS:7441)
   │  gortsplib/v5 (TLS, Describe, Setup, OnPacketRTP)
   ▼
Source.track (pion TrackLocalStaticRTP, H.264)
   │  geteilt zwischen allen PeerConnections (heute: maximal eine)
   ▼
Server (POST /offer, Content-Type: application/sdp)
   │  pion/webrtc/v4
   ▼
Browser-Tab  →  <video>
```

Public-API heute (`carvilon.local/stream`):

```go
src, _ := stream.NewSource(stream.SourceOptions{
    RTSPURL:               os.Getenv("CARVILON_STREAM_RTSP_SOURCE"),
    InsecureSkipTLSVerify: true, // UDM-Cert ohne IP-SAN — Spike-only
})
_ = src.Start(ctx)
defer src.Close()

srv, _ := stream.NewServer(stream.ServerOptions{
    Source: src,
    Addr:   ":8555",
})
_ = srv.ListenAndServe()
```

Bewusst noch **kein** `VideoSource`-Interface. `Source` ist konkret, hat aber
eine Form, aus der das Interface in Schritt 4 ohne Bruch herausgehoben werden
kann.

## Bekannte Stolpersteine

- **TLS ohne IP-SAN.** UDM-Cert hat kein IP-SAN, Go's Standard-Verify schlägt
  fehl. Der Spike läuft mit `InsecureSkipTLSVerify: true`. Später: gegen die
  UDM-CA pinnen wie der carvilon-UA-Client.
- **`rtsps://` + `?enableSrtp`.** Das `rtspx://`-Schema aus alten
  go2rtc-Notizen ist go2rtc-spezifisch und nicht gortsplib-tauglich. gortsplib
  bekommt `rtsps://`. Falls die SRTP-Aushandlung zickt: als Befund melden, nicht
  stundenlang forcieren.
- **Drop statt Buffer.** Im Single-Viewer-Spike unkritisch — pion's
  `TrackLocalStaticRTP.WriteRTP` ist fire-and-forget. Beim Fan-Out (Schritt 2)
  wird die Drop-Policy explizit gebaut.
- **Sicherheit.** RTSPS-URL nur per Env-Var. Niemals ins Repo. `.gitignore`
  deckt `.env` ab; `.env.example` liefert einen Platzhalter.

## Dependency-Doktrin

Top-Level-Abhängigkeiten sind ausschließlich:

- `github.com/bluenviron/gortsplib/v5`
- `github.com/pion/webrtc/v4` (und transitiv `github.com/pion/rtp`)

Weitere Dritt-Libs vorher mit dem Stream-Chat klären.
