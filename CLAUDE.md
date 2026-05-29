# CLAUDE.md - streaming-server Repository

**Repo:** CARVILON video stream server (Go)
**Project:** CARVILON intercom platform
**Last updated:** 25 May 2026, end of Stream season 1 (go2rtc replaced)
**Track:** Stream-Server chat - separate from the carvilon-server repo
(master chat) and the display_app repo (ESP chat)

> Language policy: all source, comments and documentation are English. The
> chat workflow is German (JARVIS style, address "Sir", umlauts, no em dashes).

---

## 1. Overview

streaming-server is the standalone video media layer of the CARVILON intercom
platform. It pulls the camera stream from a UniFi Intercom over RTSPS (H.264
High Profile, optionally SRTP/SDES encrypted), and delivers it to multiple
consumer types simultaneously via a fan-out. It replaces go2rtc (AGPL) with an
own MIT-licensed implementation.

It is a DUMB MEDIA LAYER by design: it knows cameras and profiles, but NO
tenants, users, permissions or tokens. All authorization logic lives in the
separate carvilon-server (master chat). This separation is deliberate and must
be preserved.

```
UniFi world (UDM SE + UA Intercom)                <- source
   |  RTSPS H.264 High Profile (optionally ?enableSrtp / SDES)
   v
streaming-server (this repo, RPi 192.168.1.42:8555)
   - RTSPS+SRTP pull, own H.264 depacketizer
   - fan-out: one decode -> N encodes, lazy
   - MJPEG (HTTP)      -> ESP indoor monitor
   - WebRTC (h264)     -> browser / WebViewer (via carvilon proxy)
   - H.264-CBP         -> /stream/h264
   |
   +-- carvilon-server (master chat repo) proxies WebRTC signalling +
       ESP MJPEG, adds auth. Reads /stream/stats for the admin.
```

---

## 2. Hardware / Environment

```
DEV NETWORK:
   UDM SE:           192.168.1.1
   Raspberry Pi:     192.168.1.42 (stream host, ssh alias `rpi`,
                     user sash710, hostname `carvilon`)
   Windows desktop:  192.168.1.187 (build machine, cross-compile arm64)
   ESP32-P4:         192.168.1.28 (indoor monitor, pulls MJPEG directly)
   Main camera:      679573e101080b03e4000424 (UA Intercom)
   plus 3x AI 360 + 1x G3 Flex

PORTS:
   stream server:    8555  (go2rtc used to be 1984)
   carvilon-server:  9080  (master chat, API/SSE)

PROTECT API:
   Header X-API-KEY, path /proxy/protect/integration/v1/
```

---

## 3. Software Stack

```
HOST PC (Windows, build machine):
   Go toolchain, PowerShell
   Project: C:\Projects\UniFi\streaming-server
   Cross-compile to RPi: GOOS=linux GOARCH=arm64

DEPENDENCIES (all MIT):
   gortsplib v4      RTSP/RTSPS pull (bluenviron, MIT - NOT mediamtx/AGPL)
   pion/webrtc v4    WebRTC output (h264 passthrough)
   modernc.org/sqlite pure-Go SQLite, no cgo
   ffmpeg            external subprocess (MJPEG + H.264-CBP transcode)
   stdlib            net/http ServeMux (Go 1.22), crypto/* for SRTP

RASPBERRY PI:
   carvilon-stream-rpi binary (arm64), runs in foreground
   state/stream.db (profile persistence)
```

---

## 4. Directory Structure

```
streaming-server/
   cmd/
      spike/            main entry (dev server). S2: to become Cobra
                        root with edge/cloud subcommands.
      genkey/           (if present) secrets key generator
   internal/
      h264/             own RFC-6184 depacketizer (UniFi sends mode-2
                        despite mode-1 SDP declaration)
      hub/              fan-out: one source -> N subscribers, lazy,
                        drop-not-buffer, IDR cache
      mjpeg/            ffmpeg-subprocess MJPEG encoder + multipart server
      h264esp/          H.264-CBP transcode path (/stream/h264)
      profile/          stream profiles (codec/fps/resolution/encryption)
      sourcereg/        source registry, pull key {CameraID,Quality,Encryption}
      store/            SQLite profile persistence (modernc.org/sqlite)
      stats/            client tracking + /stream/stats (/proc CPU)
      unifiapi/         ListCameras via Protect integration API
      unifi/            RTSPS pull, SRTP/SDES decryption
   streambackend/       mirror of the carvilon seam (StreamBackend iface)
   docs/                repo docs (architecture, wire-format, security,
                        profile-api, ADRs, per-fix analysis docs)
   state/               runtime (stream.db) - gitignored
```

---

## 5. Key Code Paths

### 5.1 Source pull + decryption (internal/unifi)

```
1. unifiapi.ListCameras -> RTSPS URL for camera (token redacted in logs)
2. gortsplib dials RTSPS. UDM cert has no IP-SAN -> InsecureSkipVerify +
   custom VerifyPeerCertificate against pinned CA.
3. SDP read. If a=crypto present (count=3) -> SRTP/SDES mode.
4. Own H.264 depacketizer (internal/h264) handles mode-2 packets.
5. SRTP: AES-CM via crypto/aes+cipher.NewCTR, HMAC-SHA1, ROC handling.
   NOT pion/srtp (its ~2KB packet limit rejects UniFi's large packets).
   Master key wiped from heap after session-key derivation.
```

### 5.2 Fan-out (internal/hub)

```
- One pull per key {CameraID, Quality, Encryption}.
- LAZY: hub created idle; source starts on first subscriber, stops on last.
- One decode feeds N encoders (mjpeg_bal, intercom_web, ...).
- Drop-not-buffer on slow consumers, small channels (cap=2), IDR cache for
  fast joiners.
```

### 5.3 Encoders

```
internal/mjpeg:   ffmpeg subprocess, byte-exact multipart/x-mixed-replace.
   ffmpeg args (hard-won):
     -use_wallclock_as_timestamps 1   (input timing, S6-04)
     -flags +bitexact                 (kill COM marker, ESP HW decoder, S6-06)
     -fflags +nobuffer -flags +low_delay  (latency, S6-07)
     -vf fps=N,scale=W:H              (even input sampling, S6-13 - fps BEFORE
                                       scale; never -r N at output)
internal/h264esp: H.264-CBP transcode (/stream/h264), same fps-filter rule.
internal/webrtc (in cmd/spike server): pion h264 passthrough, NO ffmpeg.
```

### 5.4 Profiles + stats

```
internal/profile + store: 11-field schema, snake_case, SQLite-persistent.
   GET /api/profiles (array), PUT/DELETE /api/profiles/{name}.
   DisallowUnknownFields - send exactly the 11 fields.
internal/stats: GET /stream/stats. clients (all codecs incl. WebRTC since
   S6-15), avg_fps, source_fps, avg_bitrate_kbps, frames_sent/dropped,
   transcoder_cpu_percent (/proc).
```

---

## 6. Encryption (source property, global - S6-14)

```
encryption (tls/srtp) is a SOURCE property (camera<->server hop), set GLOBALLY
via UNIFI_ENCRYPTION env. It is NOT a per-profile property.
   tls  = TLS tunnel, plain RTP inside (a=crypto count=0). Default.
   srtp = additionally SDES-encrypted media packets (a=crypto count=3),
          server decrypts.
The per-profile `encryption` field is DISPLAY-ONLY (mirrors the active global
mode); it does NOT steer. It stays in the schema for stability but is ignored
for the pull. (S6-12 wrongly made it steer per profile; S6-14 corrected this.)
The consumer hop is independent: MJPEG over HTTP, WebRTC over its own DTLS-SRTP.
In a pure LAN, tls is sufficient; srtp matters for external/cloud access.
```

---

## 7. Build / Deployment

```
BUILD (Windows desktop):
   cd C:\Projects\UniFi\streaming-server
   $env:GOOS="linux"; $env:GOARCH="arm64"
   go build -o carvilon-stream-rpi ./cmd/spike
   $env:GOOS=""; $env:GOARCH=""        (reset afterwards)

DEPLOY (RPi):
   ssh rpi
   pkill -f carvilon-stream-rpi         (else scp "dest open failure")
   # (copy from desktop:)  scp carvilon-stream-rpi rpi:~/
   UNIFI_NVR_HOST=192.168.1.1 UNIFI_API_KEY=<key> \
     [UNIFI_ENCRYPTION=srtp] ./carvilon-stream-rpi

Runs in FOREGROUND (dies on SSH window close). systemd service = open TODO.
Build artifact /carvilon-stream-rpi is gitignored.
```

---

## 8. Working With the Other Chats

```
MASTER CHAT (carvilon-server, :9080):
   - proxies WebRTC signalling: POST /webviewer/offer -> our /offer
   - proxies ESP MJPEG with bearer auth
   - reads GET /stream/stats for the admin consumer column
   - builds admin profile CRUD against our 11-field schema
   - issues all tokens / owns all auth (we never do auth)

ESP CHAT (display_app, indoor monitor):
   - pulls MJPEG directly: GET /api/stream.mjpeg?src=mjpeg_bal (auth-free)
   - needs the COM-marker fix (we provide it)
   - can do hardware TLS (ESP S1) - usable for an encrypted ESP hop later

Coordination docs: BRIEFING-STREAM-SXX-NN (to Claude Code),
   ANTWORT/SYNC/FRAGE-AN-MASTER-CHAT / -AN-ESP-CHAT (to the other chats).
   Always as .md files, never inline (nested code blocks break chat rendering).
```

---

## 9. Conventions (hard rules)

```
- Conventional Commits (type(scope): description).
- Claude Code commits LOCALLY, never pushes. Sascha pushes manually.
  Push after each milestone, not only at season end.
- CC reads the REAL code before changing; reports finding first, then fixes.
  Measure, don't guess.
- stdlib default; new dep only if needed, MIT/Apache only, NEVER AGPL
  (go2rtc + mediamtx are AGPL = excluded).
- Never change version numbers in config files without approval.
- Token / API key / RTSPS URL NEVER logged or committed.
- MEASURE RULE: before any test, ask which server runs, on which host/port,
  fresh or old. Never assume a server is stopped.
```

---

## 10. Performance Benchmarks (end of Stream season 1)

```
mjpeg_bal (800x1280 @ 12 fps q:v6), one ESP viewer:
   avg_fps      ~12 (even, frames_dropped=0)
   bitrate      ~4.6-5.3 Mbit/s
   encode CPU   ~4% (after S6-13 fps-filter; was ~24% before)

intercom_web (h264 passthrough WebRTC), one browser viewer:
   ~20 fps, ~6 Mbit/s, frames_dropped=0

Latency: on go2rtc level (low_delay + small buffers, S6-07).
Source fps varies (UniFi dynamic): seen 15-30 fps depending on scene.
```

---

## 11. Season History

```
SEASON 1 (22-25 May 2026):
   go2rtc REPLACED. Own MIT server: RTSPS+SRTP pull, own H.264 depacketizer,
   lazy fan-out, MJPEG + WebRTC + H.264-CBP. SRTP/SDES cracked (was NOT MIKEY)
   as a global toggle. Latency on go2rtc level, streaks fixed, stats for all
   codecs incl. WebRTC. encryption corrected from per-profile to global.
   go2rtc removed from the RPi. 24 commits, all pushed.
   Key insights: SDES not MIKEY; latency = oversized buffer; streaks = bursty
   drop vs even fps-sampling; encryption belongs to the source not the profile.

SEASON 2 (planned):
   Cloud layer (local-first). VPS role: WHIP ingest + pion fan-out + WHEP
   egress + embedded pion/turn. RPi edge: lazy WHIP upstream. One codebase,
   two roles (edge/cloud) via Cobra subcommands. With master + Android chat.
   See STREAM-CHAT-HANDOVER-2.
```

---

*End of CLAUDE.md for streaming-server. Last updated 25 May 2026, end of
Stream season 1.*
