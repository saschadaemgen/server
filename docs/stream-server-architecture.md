# Stream-Server Architecture (CARVILON video media layer)

**Status:** Updated end of Stream season 2 (31 May 2026). Living document,
extend per season. Counterpart to the carvilon-server architecture.md (master
chat) and esp-architecture.md (ESP chat), but for the video media layer.
Sibling docs in this track: stream-server-setup-notes.md,
stream-server-wire-format.md, and stream-server-decisions.md (the WHY /
learnings log).
**Scope:** internal architecture of the standalone stream server. MIT-licensed
(commercial component; the ESP firmware is a separate AGPL component).
**Repo:** `C:\Projects\UniFi\streaming-server`, branch `main`.

> Language policy: all source, comments and docs are English. Chat workflow
> German (JARVIS style).

## 1. Role in the overall system

The stream server is the **media layer**: it pulls from the camera and delivers
to consumers. It is deliberately DUMB - it knows cameras and profiles, but no
tenants, users, permissions or tokens. All authorization lives in the
carvilon-server (master chat). This keeps the media layer reusable and hardware-
independent for the future.

```
UniFi (UDM SE + UA Intercom)              <- source
   |  RTSPS H.264 High Profile (optionally ?enableSrtp / SDES)
   v
stream server (RPi 192.168.1.42:8555)
   - pull + decrypt + depacketize
   - fan-out (one decode -> N encodes)
   - MJPEG / WebRTC / H.264-CBP out
   |
   +-- carvilon-server (:9080) proxies + adds auth + reads /stream/stats
   |
   +-- ESP indoor monitor pulls MJPEG directly (LAN)
```

## 2. Module architecture

```
internal/unifi      source pull (gortsplib RTSPS) + SRTP/SDES decryption
internal/h264       own RFC-6184 depacketizer (UniFi sends mode-2 despite
                    mode-1 SDP)
internal/hub        fan-out: one source -> N subscribers, lazy lifecycle,
                    drop-not-buffer, IDR cache for fast joiners
internal/sourcereg  source registry; pull key {CameraID, Quality, Encryption}
internal/mjpeg      ffmpeg-subprocess MJPEG encoder + multipart server
internal/h264esp    H.264-CBP transcode path (/stream/h264)
internal/profile    stream profile model (codec/fps/resolution/encryption)
internal/store      SQLite profile persistence (modernc.org/sqlite, no cgo)
internal/stats      client tracking + /stream/stats (/proc CPU)
internal/unifiapi   ListCameras via Protect integration API
streambackend/      mirror of the carvilon seam (StreamBackend interface)
cmd/streaming-server  dev server entry; -role=edge|cloud (S2-02, stdlib flag)
```

Layer rule: the encoders know nothing about auth; the source layer knows
nothing about consumers; the hub couples them.

## 3. Data flow (pull-to-consumer)

```
1. A consumer requests a profile (ESP pulls mjpeg_bal; browser pulls
   intercom_web via the carvilon proxy).
2. sourcereg resolves the pull key {CameraID, Quality, Encryption}. If a hub
   for that key exists, the consumer joins it (shared pull). Otherwise a new
   idle hub is created.
3. On the FIRST subscriber, the hub starts the source: unifi dials RTSPS,
   decrypts SRTP if active, depacketizes H.264.
4. One decode feeds all encoders bound to that hub (fan-out). Each profile's
   encoder produces its own output (MJPEG / WebRTC / H.264-CBP).
5. On the LAST subscriber leaving, the hub stops the source (no client =
   no camera load).
```

## 4. Pull-sharing invariant (S6-12 / S6-14)

```
Profiles that share {CameraID, Quality, Encryption} share ONE pull (one
decode, N encodes). Different Quality OR Encryption -> a different key -> a
separate pull.
Since S6-14, Encryption in the key comes from the GLOBAL setting (env), not
per profile - so in normal operation all profiles of one camera/quality share
one pull. Encryption stays in the key for mixed-mode safety (cheap insurance).
```

## 5. Encryption model (source property)

```
encryption (tls/srtp) is a property of the CAMERA->SERVER hop, set globally.
   tls  = TLS tunnel, plain RTP inside (default; sufficient in a LAN)
   srtp = additionally SDES-encrypted media (server decrypts); for external
          / cloud access
The consumer hop is independent (MJPEG/HTTP, WebRTC/DTLS-SRTP). The per-profile
encryption field is display-only since S6-14.
```

## 6. Encode rules (hard-won, S6 - do NOT violate)

```
1. fps reduction (source_fps > target_fps) MUST be an even input sample:
   -vf fps=N,scale=W:H:flags=fast_bilinear (fps BEFORE scale). Never -r N at
   the OUTPUT - that only throttles the container clock and causes bursty
   channel-full drops (= streaks on the ESP). (S6-13; S3-01 added the
   fast_bilinear scaler - the default bicubic downscale of the 1200x1600
   source was the real throughput bottleneck, see D-0003)
2. -flags +bitexact to kill the ffmpeg COM marker (the P4 HW decoder fails on
   it). Note: -flags (codec) != -fflags (format). (S6-06)
3. -fflags +nobuffer + small channels (cap=2) for latency. Do NOT enlarge
   buffers to fix drops - that regresses latency. (S6-07)
   NOTE (S2-16): -flags +low_delay was REMOVED - it disables the decoder's
   multi-core threading and starves the 1200x1600 decode (single-core stall ->
   encoder-input backup -> P-frame loss -> stutter at GOP 105). See
   stream-server-decisions.md D-0002. Do NOT reintroduce it; the
   NoCodecLowDelay test canary guards this.
   NOTE (S3-01): -threads 4 before -i makes the all-core decode of the
   1200x1600 source explicit. (D-0003)
4. -r 30 before -i: hand ffmpeg the camera's true constant 30 fps as an even
   1/30 PTS base, so the fps filter samples evenly. Replaced
   -use_wallclock_as_timestamps 1 (S6-04), whose arrival-time PTS were clumpy
   on the bursty raw H.264 and overran the encoder queue above 12 fps.
   (S3-01, D-0003)
5. Encoder spec is frozen at ffmpeg spawn. A profile change needs a fresh
   encode: compare spec on subscribe, retire+restart on mismatch. (S6-10)
```

**Note (D-0004):** the achievable MJPEG output fps is capped by the SOURCE
fps. The UniFi camera in FPS-Auto mode delivers fewer frames in low light
(longer exposure), so output fps drops at night regardless of encoder
settings. The UI's "FPS 30" is the configured maximum, not the delivered
rate. Diagnose via source_fps in /stream/stats and cross-check a passthrough
profile (e.g. intercom_web) before suspecting the transcode. See
stream-server-decisions.md D-0004.

## 7. Stats model

```
GET /stream/stats. One client registry for ALL codecs (since S6-15 WebRTC is
included). WebRTC presence is registered in the feed goroutine and
de-registered on teardown + a 30s idle watchdog (WebRTC teardown is never
free). global.clients = sum over all codecs. Per profile: clients, avg_fps,
source_fps, avg_bitrate_kbps, frames_sent/dropped. global:
transcoder_cpu_percent (/proc).
```

## 8. Open architecture points (S2+)

```
- Two roles, one binary (S2): Cobra root with subcommands edge (RPi: pull +
  lazy upstream) and cloud (VPS: upstream ingest + fan-out + egress).
  Shared code in internal/common.
- Cloud uplink (S2): WHIP (RFC 9725) over the existing pion stack; RPi is
  WHIP-client (outbound -> CGNAT-friendly). SRT fallback (datarhei/gosrt) for
  bad radio links.
- Cloud fan-out (S2): pion broadcast pattern (TrackLocalStaticRTP), no
  re-encode, no heavy SFU lib. intervalpli for keyframes. WHEP egress.
- TURN (S2): embed pion/turn on the VPS, port 443/TCP.
- ffmpeg removable on the edge WebRTC path (pion takes H.264 directly; only
  RTP repacketizing needed). MJPEG path keeps ffmpeg.
- systemd service on the RPi (currently foreground).
- Sales features (later): watermark (ffmpeg overlay filter in the encode
  step; doubles as free-tier limiter), event recording on doorbell, live-grid.
- Two-way audio (S3+): door->viewer audio (track exists, not pulled);
  viewer->door audio is a separate RE topic.
- Merge stream binary into the carvilon binary (GOPRIVATE): DEFERRED while
  both products are developed separately. Open-Core via build tags
  (ADR-STREAM-01) keeps this a configured step for later.
```

---

*Living document. Last: 2026-05-31 (Stream season 3, S3-01). See
stream-server-decisions.md for the encode-throughput findings (D-0002
low_delay, D-0003 even-rate input + fast_bilinear scaler).*
