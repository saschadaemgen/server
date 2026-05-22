# S6-07: MJPEG latency diagnosis vs. go2rtc

## Symptom

`http://localhost:8555/api/stream.mjpeg?src=mjpeg_bal` had ~2 s of
perceived latency in the browser. Reproduced server-side (not ESP).
go2rtc on the **same** UA-Intercom RTSP stream did not have this lag —
that's the calibration point: we don't optimise into the blue, we
optimise *to go2rtc*.

## Suspect-by-suspect comparison

### A. ffmpeg input flags

go2rtc's source (`internal/ffmpeg/ffmpeg.go`, defaults map) builds:

```
"rtsp": "-fflags nobuffer -flags low_delay -timeout {timeout} \
         -user_agent go2rtc/ffmpeg -rtsp_flags prefer_tcp -i {input}"
```

Two latency flags, applied at the INPUT side:

| Flag                  | Level             | Effect                                |
| --------------------- | ----------------- | ------------------------------------- |
| `-fflags nobuffer`    | format / demuxer  | don't buffer at demuxer level         |
| `-flags low_delay`    | codec / decoder   | decoder skips reorder-buffer fill     |

**Our state before S6-07:** had `-fflags +nobuffer` (S6-04), did NOT
have `-flags +low_delay`. Identified as the half-fix that explained
the persistent (not just startup) latency: even with `-bf 0` H.264
profiles, the libavcodec decoder still allocates a small reorder
buffer that adds 1-2 frames of per-session lag unless `low_delay` is
on.

go2rtc does NOT set `-probesize 32` / `-analyzeduration 0` either —
they accept the ffmpeg default (5 s / 5 MB). We match.

### B. Per-subscriber buffer depth — THE big one

| | go2rtc | us (pre-S6-07) | us (post-S6-07) |
|---|---|---|---|
| MJPEG subscriber buffer | **0** (synchronous TCP backpressure) | 30 | 2 |
| @ 15 fps source rate    | 0 ms                    | **2000 ms (!)** | ~130 ms |

go2rtc has no per-consumer queue: a slow client blocks the writer
goroutine on `net.Conn.Write`. That throttles upstream packet
delivery for that consumer only — TCP is the buffer.

Our design uses non-blocking sends with drop-on-overflow so one wedged
client cannot stall the encoder for the other viewers. We **keep**
that design (the right call for fan-out) but the queue depth was
sized as if it were a buffer (30 frames) rather than just a jitter
absorber. 2 frames is plenty for a single-frame TCP write hiccup
without being able to ever build up perceptible lag.

This is the dominant 2-second contributor. The math hits the briefing
number exactly: 30 frames ÷ 15 fps = 2.0 s.

### C. Encoder channel depths

Same logic, smaller scale. Before/after:

| Channel        | Before | After |
| -------------- | ------ | ----- |
| Encoder input  | 8      | 2     |
| Encoder output | 4      | 2     |

In-process channels in front of and behind ffmpeg. The encoder runs
near-line-rate, so these only need to absorb a single-frame jitter
spike. 12 frames of slack across these two alone was ~0.8 s @ 15 fps;
4 frames is ~0.27 s, and in practice they stay near-empty.

### D. Upstream H.264 hub (internal/hub)

**Unchanged.** Still 30-frame default subscriber buffer.

Reasoning: the H.264 hub serves WebRTC (intercom_web) AND the MJPEG
transcoder AND the H.264-CBP transcoder simultaneously. Reducing its
buffer would change WebRTC's behaviour, which is a separate concern.

The MJPEG forwarder reads from this buffer non-blockingly and drops
into the (now-tight) encoder input — the upstream chan stays near
empty in any sustained-rate scenario, so this 30-deep buffer doesn't
contribute to MJPEG latency in practice.

### E. HTTP write path

go2rtc flushes after every JPEG part (`pkg/mjpeg/writer.go`). We do
the same in `handleMJPEG`. No change needed.

## Expected effect

| Stage                  | Worst-case slack (pre) | Worst-case slack (post) |
| ---------------------- | ---------------------- | ----------------------- |
| Encoder input          | 8 / 15 = 0.53 s        | 2 / 15 = 0.13 s         |
| Encoder output         | 4 / 15 = 0.27 s        | 2 / 15 = 0.13 s         |
| MJPEG subscriber buf   | 30 / 15 = 2.00 s       | 2 / 15 = 0.13 s         |
| **Sum (worst case)**   | **≈ 2.8 s**            | **≈ 0.4 s**             |

Plus `-flags low_delay` removes the decoder reorder-buffer overhead
(1-2 additional frames per session).

The 2.8 → 0.4 worst-case matches the observed 2 s lag dropping to a
~go2rtc-comparable level. Real wallclock latency will be lower than
worst-case because the queues stay mostly empty under normal load.

## Test lockdown

- `TestBuildFFmpegArgs_HasLowDelayFlags` — fails LOUDLY with a
  briefing-anchored message if `-fflags +nobuffer` or `-flags
  +low_delay` are removed, or if they appear after `-i pipe:0`
  (wrong context).
- `TestEncoderDefaults_LowLatencySized` — caps `defaultInputBuf` and
  `defaultOutputBuf` at 4 each so a "channels look small, surely we
  can bump them" rewrite gets caught.

## Things NOT done (briefing § 4 nebenbefunde)

The briefing flagged two side-findings that are explicitly out of
scope for S6-07:

1. UA-Intercom RTSPS API may offer a higher-fps stream — to be looked
   at AFTER latency. Source-side fps cap is not blocking the door.
2. `/api/profiles` GET returns `cameraID` / `encodeQuality` (camel),
   PUT expects `camera_id` / `encode_quality` (snake). Inconsistent.
   Small follow-up — noted, not fixed here.

## Verification on a live server

```sh
# Latency feel test:
# Open http://localhost:8555/api/stream.mjpeg?src=mjpeg_bal in a browser.
# Wave a hand in front of the camera. The browser image should follow
# without the previously-observed ~2 s lag. Compare against go2rtc on
# the same stream if available — they should feel comparable.

# Args inspection:
go test -race -run TestBuildFFmpegArgs ./internal/mjpeg/...

# Stats during a live measurement: per-client avg_fps should match
# the source_fps within a fraction of a frame, no longer drift behind:
curl -s http://localhost:8555/stream/stats | jq '.profiles'
```
