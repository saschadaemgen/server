# Stream-Server Decisions & Learnings (CARVILON video media layer)

**Status:** Started end of Stream season 2 (31 May 2026). Living document,
prepend new entries on top. Counterpart to the three state docs
(stream-server-architecture.md, -setup-notes.md, -wire-format.md): those
describe HOW the server is built; THIS one records WHY, and what we learned
the hard way (including dead ends, so nobody walks them again).

> Language policy: all source, comments and docs are English. Chat workflow
> German (JARVIS style).

**How to read this:** each entry is a decision or a hard-won lesson. Format:
what we decided/found, the context, the alternatives we rejected and why, the
consequence. Newest on top. When an entry overturns a rule in another doc, it
says so explicitly and the other doc is corrected to point here.

---

## D-0003 (S3-01, 31 May 2026): even 30 fps input rate + decode threading + fast_bilinear scaler

**Decision:** Three changes to the MJPEG encode path (`internal/mjpeg`), all
measured on the RPi4 against the live 1200x1600 High Profile source:

- Replace `-use_wallclock_as_timestamps 1` with **`-r 30` before `-i`** (an
  even 1/30 PTS base for the raw H.264 demuxer).
- Add **`-threads 4`** before `-i` (decode-side, all RPi4 cores).
- Append **`flags=fast_bilinear`** to the scale filter
  (`-vf fps=N,scale=W:H:flags=fast_bilinear`).

**This corrects architecture.md Section 6, rule 4 (S6-04).** That rule
mandated `-use_wallclock_as_timestamps 1` "for honest PTS"; it rested on a
wrong measurement (camera ~15-17 fps) and now points here.

**Context / symptom:** After the low_delay fix (D-0002) MJPEG was stable at 12
fps but would not scale up - raising mjpeg_bal to 15/20/25 fps brought back
stutter and GOP smear even though ffmpeg CPU sat at ~7%. CPU was clearly not
the limit, so something upstream was starving the pipeline.

**Root cause (measured, proven):**
- The camera does NOT run ~15-17 fps; it delivers a **constant 30 fps** (UniFi
  UI + ffprobe on a live dump). S6-04's wallclock PTS stamped each frame with
  its pipe-arrival time, but raw H.264 arrives bursty (large keyframes, GOP
  ~105), so arrival-time PTS were clumpy and the `fps` filter emitted frames
  in clumps. At 12 fps the heavy decimation hid it; at 15-25 fps the clumps
  overran the encoder input queue -> lost P-frames -> ~5 s GOP smear. `-r 30`
  hands ffmpeg the true even rate, so the `fps` filter samples evenly
  (verified: exactly 342 frames for fps=20 over 17 s).
- The **default (bicubic) scaler** doing the 1200x1600 -> profile downscale
  was the actual throughput bottleneck. `fast_bilinear` is visually
  indistinguishable for a downscale yet far cheaper: fps=20 went **1.23x ->
  2.61x**, fps=25 runs at 2.42x. This is what makes 20-25 fps viable; 12 was
  the prior ceiling.
- `-threads 4` lets the 1200x1600 decode use all four RPi4 cores explicitly.

**Rejected / not the cause:** enlarging buffers (regresses latency - rule 3 /
D-0001); blaming CPU (~7%, never the limit); keeping wallclock and adding `-r`
at the OUTPUT (output `-r` is the S6-13 throttling footgun - the rate fix
belongs on the INPUT side only).

**Consequence:** ESP live went ~11 -> ~13 fps with `frames_dropped 0` at the
server, ffmpeg ~8% CPU, no more pixelation/smear on motion. WebRTC is
untouched (H.264 passthrough, no ffmpeg decode). The S6-04 expectation is
inverted into the `NoWallclockEvenRate` test canary, which fails if wallclock
returns or if `-r 30` / `-threads 4` go missing; the OutputArgs test asserts
`fast_bilinear`.

**Lesson:** when CPU is low but a transcode still can't keep up, the
bottleneck is a pipeline stage starving the rest (here the scaler), not raw
compute. And measure the source rate before designing around it - wallclock
PTS is the wrong tool for a bursty constant-rate stream.

---

## D-0002 (S2-16, 31 May 2026): `-flags +low_delay` is a THROUGHPUT footgun, removed

**Decision:** Removed `-flags +low_delay` from the ffmpeg decode args in both
`internal/mjpeg/encoder.go` (buildFFmpegArgs) and `internal/h264esp/encoder.go`.
`-fflags +nobuffer` and `-use_wallclock_as_timestamps 1` stay. The test canary
`HasLowDelayFlags` was inverted to `NoCodecLowDelay` - it now fails loudly if
anyone reintroduces the flag.

**This overturns architecture.md Section 6, encode rule 3 (S6-07).** That rule
recommended `+low_delay` "for latency". It was wrong from the start - a
latency micro-optimization that cost the entire decode throughput. The rule in
architecture.md has been corrected to point here.

**Context / symptom (cost ~2 days to chase):** The MJPEG stream (ESP and
browser, `/api/stream.mjpeg?src=mjpeg_bal`) ran fine for ~10s after a fresh
start, then degraded into stutter with growing latency on motion. WebRTC
(WebViewer, same camera, same moment) stayed perfectly smooth throughout.

**Root cause (measured, proven):** `AV_CODEC_FLAG_LOW_DELAY` disables the
H.264 decoder's internal multi-core threading (it forces in-order, no-reorder,
single-threaded decode). On a light stream this is invisible. But this camera
delivers **1200x1600 High Profile, tbr 60, GOP ~105** (keyframe only every ~5s,
measured: 6 I-frames and 627 P-frames over 633 AUs, NO B-frames). With
low_delay, ffmpeg decoded on a single core, fell behind the source rate in live
operation, the `encoder input` channel backed up, P-frames were dropped, and at
GOP 105 EVERY dropped P-frame destroys the picture for up to 5 seconds (until
the next keyframe). That is the stutter.

WebRTC is immune because it does NOT decode - it passes the H.264 through
(per-AU WriteSample), the browser decoder reorders and decodes. The failure was
specific to the ffmpeg transcode path.

**Proof:** the same camera dump, decoded as a FILE, ran at speed=1.18x WITH
low_delay vs 1.52x WITHOUT - no decode errors either way (the stream is clean,
ffmpeg was just CPU-bound on one core). After removing the flag, an ISOLATED
binary (clean main + only low_delay-removed, no other changes) ran 18+ minutes
live with `frames_dropped: 0`, steady avg_fps 11.9, and ffmpeg CPU dropped from
100-184% to ~7.5%. ESP picture perfectly smooth the whole time.

**Rejected alternatives (all measured, all dead ends - do NOT revisit):**
- HW decode via `h264_v4l2m2m` on the RPi4: measured ~1.6x SLOWER than simply
  removing low_delay (v4l2m2m batch-pipeline overhead + latency). The RPi4 HW
  H.264 decoder exists (`/dev/video10..31`), but it is NOT the win here.
- Write-pacing in runStdin (S2-09): a pacing layer that timed stdin writes to
  PTS. The S1 binary shows the SAME stall WITHOUT any pacing, so pacing was not
  the cause; it was overhead that broke the S1 line-speed input principle.
  Reverted.
- SO_SNDBUF + drop-oldest (S2-13): a send-side / TCP-side change. It addresses
  a separate (output) latency concern but did NOT fix the picture - the bug is
  the decode stall, upstream of it. Reverted (the isolated low_delay-only fix
  is sufficient).
- Suspecting B-frames: measured, there are NONE (only I and P). Rejected.
- Suspecting CPU overload generally, the in-process merge, the shared registry,
  a camera/UDM firmware update, a fresh restart/reboot: all ruled out by direct
  test (the S1 binary `479ad09` reproduces the bug; a UDM reboot did not fix it).

**Cost:** low_delay-removed adds ~1-3 frames of decode latency. That is the
trade against multi-second stutter lags - a clear win.

**Lesson:** "WebRTC clean + MJPEG broken on the same camera" points at the
ffmpeg transcode, not the camera/network/source. And a decoder flag added "for
latency" can silently throttle threading - measure throughput, not just latency.

---

## D-0001 (S6, season 1, 22-25 May 2026): foundational encode rules (still valid)

These were established in season 1 and remain in force (see architecture.md
Section 6). Recorded here for the "why":

- **Even input sampling, not output throttling (S6-13):** `-vf fps=N,scale=W:H`
  (fps BEFORE scale) samples the source-over-target surplus evenly at the
  input. `-r N` at the output only throttles the container clock and causes
  bursty channel-full drops (= streaks on the ESP). Side effect: scale runs on
  N instead of source-fps frames -> ~60% less encode CPU.
- **`-flags +bitexact` (S6-06):** removes ffmpeg's COM marker (`FF FE
  "Lavc..."`), on which the P4 HW JPEG decoder fails. Subtlety: `-flags`
  (codec) is NOT `-fflags` (format) - only the codec-level flag removes it.
- **`-use_wallclock_as_timestamps 1` (S6-04):** gives ffmpeg honest input
  arrival timestamps, which the fps filter needs to sample evenly. Stays.
- **Encoder spec frozen at spawn (S6-10):** an ffmpeg encode freezes its spec
  when it starts. A profile change needs a fresh encode (compare spec on
  subscribe, retire+restart on mismatch), not a live reconfigure.
- **SRTP is SDES, not MIKEY (S6-11):** the ?enableSrtp SDP carries the master
  key in cleartext (a=crypto inline, 30 bytes). Decryption was hand-rolled
  (AES-CM + HMAC-SHA1) because pion/srtp's ~2KB packet limit rejects UniFi's
  large packets. Verified against RFC 3711 test vectors.
- **Encryption is a source property, not a profile property (S6-14):** it
  belongs to the camera->server hop (global env), not to a delivery profile.

**Note on source_fps:** `source_fps` in /stream/stats is a CUMULATIVE mean
since session start (`source_frames / uptime`), NOT a sliding window. It
converges upward after warmup (e.g. 24 -> 29) - this is a measurement artifact
of the averaging, NOT an accelerating camera. Judge health by `frames_dropped`,
not by a rising source_fps.

---

*Living document. Newest entry on top. Last: 2026-05-31 (Stream season 3,
S3-01: even-rate input + fast_bilinear throughput, D-0003).*
