# S6-13: ESP motion streaks — chaotic input drops, not buffer size

## Symptom

ESP pulling `mjpeg_bal` showed pixelated streaks on motion, clean
still-image. Server stats:

```
src = 29.9 fps      ← camera delivers 30 fps on the RPi
fps = 11.4          ← encoder outputs ~12 (target was 12, fine)
cpu = 24.5 %        ← not CPU-bound
```

Server log, repeating per second:

```
mjpeg: session "mjpeg_bal" encoder input: dropped 2..8; last err: "encoder input channel full"
mjpeg: session "mjpeg_bal" viewer 1:     dropped 1..6; last err: "viewer frames channel full"
```

So 30 fps was arriving, ~12 was going out, but ~18 fps was being
**dropped chaotically at the encoder input channel** rather than
**sampled evenly** before encode.

## Diagnosis (from the code, not from guessing)

Pipeline trace:

1. **Camera hub → forwarder** (`mjpeg.session.runForwarder`): reads
   AUs at ~30 fps. Non-blocking send into `encoder.Input()` channel
   of cap 2 (S6-07 default). On overflow → `dc.Record("encoder
   input channel full")`.

2. **Forwarder → ffmpeg stdin** (`runStdin`): blocking write into
   ffmpeg's stdin pipe. If ffmpeg drains stdin slower than the
   forwarder produces, the channel of cap 2 fills in ~2 / 18 = 110 ms
   and every subsequent frame goes to drop until ffmpeg catches up.

3. **ffmpeg with `-r 12` at output**: per ffmpeg docs, output `-r`
   "duplicate or drop input frames to achieve constant output frame
   rate." In practice, combined with `-use_wallclock_as_timestamps 1`
   at input and `-fflags +nobuffer -flags +low_delay`, ffmpeg's
   stdin consumer was throttled to roughly the output PTS rate.
   That backpressure propagated to our forwarder.

4. **Result**: the 12 frames per second we kept were "whichever
   ones happened to fit in the 2-slot channel when ffmpeg next
   pulled" — NOT an even sample of the 30 input frames. Uneven
   sampling = motion-dependent pixel streaks (the displayed pixels
   jump between input frames at irregular intervals).

5. **Secondary viewer-channel drops**: even when ffmpeg DID emit
   12 fps, they came in bursts (matching the bursty drain rate).
   The viewer channel of cap 2 then overflowed on top of TCP+ESP
   backpressure, adding a second round of chaotic drops.

## Why the obvious fix (bigger buffers) doesn't work

At sustained 30 → 12 fps, **every buffer fills regardless of size**.
The mismatch is structural, not transient. Bigger buffers just delay
the drop and reintroduce the S6-07 latency. The dropping has to
become EVEN, not RARE.

## Fix — single line of args, both encoders

Replace `-vf scale=W:H -r N` (output) with `-vf fps=N,scale=W:H`
(filter chain). The `fps` filter is ffmpeg's canonical even-sampling
tool: it picks the input frame whose PTS is nearest each `1/N`-second
output boundary. The filter runs INSIDE ffmpeg's filter graph, so
ffmpeg consumes stdin at line speed — no stdin throttling, no
backpressure to our forwarder.

Bonus: putting `fps=N` FIRST in the chain means `scale=W:H` runs at
the TARGET rate (12 fps), not the SOURCE rate (30 fps). The scaler
is ~60 % cheaper.

**Applied to both encoders** (`internal/mjpeg/encoding.go` and
`internal/h264esp/encoding.go`) because both share the same camera
input behavior and both encode at a target rate below the camera's
delivery rate.

## What stayed the same

- **S6-07 channel sizes** (Encoder InputBuf=2, OutputBuf=2, Hub
  SubscriberBuffer=2). The latency win is preserved. With ffmpeg
  consuming stdin at line speed now, these channels stay near-empty
  during normal operation.
- **S6-04 `-use_wallclock_as_timestamps 1`** at input. The fps
  filter relies on PTS to do its even sampling; with synthetic
  25-fps default PTS the filter would mis-sample. Wallclock PTS
  keeps the math honest.
- **All S6-06/-10/-11/-12 invariants** (`-flags +bitexact` for
  COM-marker, `-x264-params sliced-threads=0:slices=1` for h264esp,
  SRTP, encryption-per-profile). Untouched.

## Lock-down tests

- **`TestEncodeSpec_OutputArgs_FpsFilterFirst`** (mjpeg) and
  **`TestOutputArgs_FpsFilterFirstAndNoBareR`** (h264esp): assert
  the `-vf fps=N,scale=W:H` chain is present in that exact order
  AND that no standalone `-r N` exists in the args. Both regressions
  the briefing warns against (re-order, re-introduce `-r`) get
  caught at the unit-test level.
- **`TestEncodeSpec_OutputArgs_Order`** updated to the new layout.
- **`TestBuildFFmpegArgs_LayoutMatchesContract`** and friends:
  updated to the new `-vf fps=N,scale=W:H` shape; the spec values
  still flow through correctly.

`-race` clean. `GOOS=linux GOARCH=arm64` cross-compile clean.

## Local ffmpeg sanity check (no live UDM needed)

```
ffmpeg -hide_banner -loglevel error -nostats \
       -f lavfi -i "testsrc=duration=3:size=320x240:rate=30" \
       -an -vf "fps=12,scale=800:1280" \
       -c:v mjpeg -q:v 6 -y /tmp/check.mp4
ffprobe -show_entries stream=avg_frame_rate,nb_frames,duration \
        -of default=noprint_wrappers=1 /tmp/check.mp4
#   avg_frame_rate=12/1
#   duration=3.000000
#   nb_frames=36
```

Exactly 12 fps × 3 s = 36 frames. The filter does what the docs say.

## Live verification — what to watch for after restart

The briefing says the RPi server is currently running in SRTP mode;
the ESP is actively pulling. **Sascha decides** when to stop the
running server. After restart with this build:

```sh
curl -s http://192.168.1.42:8555/stream/stats | jq '.profiles.mjpeg_bal'
# Expected:
#   "source_fps":  ~30        (unchanged — camera still delivers 30)
#   "avg_fps":     ~12        (target hit smoothly, no jitter)
# Log should NO LONGER show:
#   "encoder input channel full"
# Viewer-channel drops should be near-zero unless the ESP itself
# falls behind on HTTP-read.
```

Visually: smooth motion on the ESP screen, no pixelated streaks.
The 12 fps stays at 12, but the frames are now evenly spaced.

If `encoder input channel full` drops are gone but `viewer frames
channel full` still fires, that's a downstream-TCP-to-ESP issue —
separate problem (briefing scope is only the input-side chaos).

## Scope explicitly NOT done

- **Larger buffers** (anti-fix per briefing).
- **Lower the camera fps at the source** (would need UniFi-side
  config; out of our control).
- **Server-side Go-level frame skipping** (ffmpeg's fps filter is
  the right tool for this, no need to duplicate).
- **Admin / carvilon side** — master-chat.
