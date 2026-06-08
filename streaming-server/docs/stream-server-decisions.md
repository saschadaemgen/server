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

## D-0008 (S3, egress-auth): WHEP egress token = separate HMAC key, fail-closed, verified before the cold-trigger

**Decision:** The WHEP egress (`POST /whep/{streamID}`) now requires a Bearer
egress token, verified with the EXISTING `publishtoken.Verify` against a
SEPARATE key (`CARVILON_EGRESS_TOKEN_HMAC_KEY`, 32-byte hex). The egress token
is byte-identical to a publish token (same `{sid,exp,nonce}` format) - no new
crypto; the same Verify validates it with the egress key.

**Why a separate key:** push (publish at the WHIP ingress) and pull (subscribe
at the WHEP egress) must not be interchangeable - a publish token must not
grant a pull, nor vice versa. Same format, different key: a publish-key-signed
token presented at the egress fails the signature check (proven by a
round-trip mint with the publish key -> `signature mismatch` at the egress).

**Fail closed:** with no egress key configured, every WHEP subscribe is
rejected 401 (a loud one-shot boot WARN flags the missing key). Chosen over
fail-open because the door is meant to be locked and the key is already set on
both machines; NOT fatal at boot, so the WHIP-ingress / MJPEG paths keep
running. A bad FORMAT (set but not 32-byte-hex) IS fatal, like the publish key.

**Order (security-relevant):** the 401 verify runs BEFORE the cold-start
trigger (D-0007), so an unauthorized subscriber can never force the edge to
publish. Bare 401, concrete failure class logged only (no oracle), mirroring
the ingress. The key value is never logged (only env name + byte length).

**Consequence:** external WHEP pull is gated. Open debt (see
stream-server-security.md): symmetric HMAC (not asymmetric); the token is
sid-bound but NOT client-bound (a leaked token is valid ~5 min for that sid);
no egress rate-limit yet.

---

## D-0007 (S3, WHEP cold-trigger): subscriber-driven request_publish via an Open-Core callback

**Decision:** A WHEP subscriber for a stream with no active publisher triggers
a `request_publish` to the edge, waits up to `coldPublishTimeout` (12 s) for
the publisher to dock in the hub, then attaches (201). Before this it was a
bare 404 - only the Master's loopback hook could start a publish, unreachable
to a remote client.

**Open-Core seam:** the trigger is a plain
`RequestPublishFunc func(ctx context.Context, streamID string) (edges int)`
field on `CloudSetupOptions` (stdlib types only - `context.Context` is
stdlib). The stream package does NOT import the side-channel; the Master wires
the callback to `sidechannel.Server.RequestPublish`. Mirrors the SetICEMinter
pattern, in the stream->Master direction.

**Single-flight per streamID:** simultaneous cold subscribers fire at most one
request_publish (a double trigger would be harmless - the edge publishes once).
edges==0 -> fail fast (504, no wait); timeout -> 504 (NOT 404, the trigger
ran); nil callback -> unchanged 404 behaviour.

**Lesson:** a remote client cannot reach the loopback hook; the real trigger
must be the subscriber arrival itself, coupled through an Open-Core callback so
the dumb media layer stays free of tenant/side-channel types.

---

## D-0006 (S3, telemetry): TURN/ICE telemetry as Open-Core types, IP raw+masked, no secret

**Decision:** The in-process TURN relay is a read-only data source for the
admin: `CloudServer.TURNStats()` (allocation count + a live client set) and an
`OnTURNEvent` callback (allocation created/deleted/error + auth verdict) wired
into pion's `ServerConfig.EventHandler`. Plus an optional whipclient
`OnICEState` callback (structured ICE-state transitions).

**Open-Core:** TURNStats / TURNClient / TURNEvent / ICEStateEvent carry ONLY
stdlib types (string/int/bool/time). pion's `net.Addr` is read only at the
boundary and rendered to strings - no pion/net type crosses the seam (same
rule as the ICEServer naht), so the embedding module's public build stays
pion-free. The Master persists the events (SQLite) and polls TURNStats.

**IP raw AND masked:** every event carries the mieter IP both raw and masked
(reusing the icedebug masker); the Master chooses which to store. The TURN
shared secret and the credential password NEVER appear in stats/events/logs.

**What pion does NOT provide (honest):** relayed-byte counters and a
STUN-binding count - pion/turn has no such counter (only AllocationCount +
Close + the EventHandler). Cutting a single allocation is not cleanly possible
via the public turn API. Those were deliberately not faked.

---

## D-0005 (S3, ICE/TURN): in-process pion/turn (UDP+TLS in one server), turns: on a public hostname

**Decision (the cloud ICE fix for RPi-behind-CGNAT <-> public VPS):**
- Embed **pion/turn** in the cloud setup (not a separate process). ONE pion
  server carries the UDP relay AND the TLS relay (turns:) via
  PacketConnConfigs + ListenerConfigs. The cloud peers and the edge whipclient
  each get a fresh ephemeral REST credential per allocation (HMAC over the
  shared secret, TTL 5 min); the long-term secret stays cloud-side.
- **TURN, not pure STUN:** the RPi is behind CGNAT, so srflx alone does not
  connect - a relay (TURN) is required. STUN is added as a credential-less
  entry on the SAME relay UDP port (pion answers Binding unauthenticated):
  free, no second server/port/firewall rule.
- **turns: on a public HOSTNAME, not a bare IP:** pion verifies the turns: TLS
  handshake against the system root pool with ServerName = the URL host, so a
  private-CA cert on an IP is rejected. The WHIP ingress keeps its private
  cloudca cert; the turns: leg uses a SEPARATE publicly-trusted cert
  (TURNTLSCertFile/KeyFile, e.g. Let's Encrypt for the relay hostname).
- **TURNTLSPort==0 = TLS leg OFF (opt-in);** a TLS-on setup with an unloadable
  cert is a hard error (no silent partial relay).

**Key findings (each cost a befund):**
- A relay ICE candidate is labelled by its RELAYED transport (**udp**)
  regardless of whether the client reached the relay over udp or tls/tcp. So
  "no proto=tcp candidate" is EXPECTED, not a bug; the two relay candidates are
  turn:udp + turns:tls.
- **ICEServers do NOT travel in the SDP.** The SDP answer carries the server's
  gathered candidates, not its ICEServers config. A NAT client therefore needs
  its OWN ICEServers to form srflx/relay - this is the Android-subscriber
  signalling debt (see feature-backlog).

**Consequence:** ICE reaches `connected` over the UDP relay; turns: confirmed
as the TLS allocation on :5349. The publish path (edge gets ICEServers via the
request_publish frame) connects; the subscriber media path needs the CLIENT to
carry ICEServers.

---

## D-0004 (S3-01, 31 May 2026): source fps is light-dependent - MJPEG fps tracks it

**Finding:** After S3-01 the MJPEG fps appeared to fall to ~9.7 in the evening
(vs ~13 at the ESP in the daytime test). The S3-01 fix was suspected first -
REFUTED by measurement. Cause: the UniFi Intercom in FPS-Auto mode genuinely
delivers fewer frames in low light (longer exposure). Measured: source_fps ~15
in the dark, ~20-30 in light; proven live by brightening the room - source_fps
jumped immediately from 15 to 20.4 (one variable changed, result followed).

**Proof it is the SOURCE, not the transcode:** the intercom_web stream
(h264_passthrough, no ffmpeg) showed the same ~15 fps. If even the pure
pass-through is throttled, the throttling comes from the camera, not the
server. The running ffmpeg cmdline was correct (`-r 30 -threads 4` +
fast_bilinear), so S3-01 is sound - the source simply delivered half as much.
The UniFi UI still reads "FPS 30", which is the configured maximum, not the
delivered rate. Vendor guides confirm Protect lowers the frame rate in low
light in FPS-Auto mode (12-15 fps is the recommended night value for entry
areas).

**Consequence:** achievable MJPEG output fps is capped by the source fps. 20-25
fps is a DAYLIGHT promise. For fixed fps at night, switch the camera in UniFi
(Settings -> Video) from FPS-Auto to a fixed value - price: more motion blur /
noise at night. This is a CAMERA setting, not a server concern; ffmpeg cannot
manufacture frames the source never sends.

**Lesson:** for "too few fps", check source_fps in /stream/stats FIRST and
cross-check a passthrough profile (intercom_web) before suspecting the
transcode. A passthrough and a transcode showing the same low rate localizes
the cause to the source in a single step.

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

*Living document. Newest entry on top. Last: end of Stream season 3 (cloud
arc: ICE/TURN D-0005, telemetry D-0006, WHEP cold-trigger D-0007, egress-auth
D-0008).*
