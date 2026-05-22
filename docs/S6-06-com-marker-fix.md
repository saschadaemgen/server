# S6-06: COM-Marker fix (ESP-P4 HW JPEG decoder compatibility)

Live-confirmed integration finding from the ESP-Chat against `.187:8555`:
MJPEG frames arrived (~51 KB/frame), transport was clean, but the ESP
hardware JPEG decoder rejected every frame with:

```
jpeg.decoder: jpeg_parse_com_marker: COM marker data underflow
              for header_size: 6
jpeg_decoder_process: jpeg parse marker failed
STREAM: Decode failed: ESP_ERR_INVALID_ARG, size=51829
```

Root cause: libavcodec's MJPEG encoder writes a COM segment (JPEG
marker `0xFFFE`) containing its version string, e.g. `Lavc62.28.101`.
The Espressif-P4 HW JPEG driver does not tolerate this segment.

## Before — proof the marker was there

ffmpeg args (S6-04 state, pre-S6-06):

```
-an -vf scale=800:1280 -r 12 -c:v mjpeg -q:v 6 -f mjpeg
```

First 48 bytes of resulting JPEG (synthetic testsrc input, same args
as the live spike):

```
00000000: ffd8 ffe0 0010 4a46 4946 0001 0200 0001  ......JFIF......
00000010: 0001 0000 fffe 0010 4c61 7663 3632 2e32  ........Lavc62.2
00000020: 382e 3130 3100 ffdb 0043 0008 0c0c 0e0c  8.101....C......
                    ^^^^ ^^^^ Lavc62.28.101\0
                    COM  len=16 (the segment that crashes ESP-P4 HW decoder)
```

## Fix — one flag, codec level

Add `-flags +bitexact` to `internal/mjpeg.EncodeSpec.OutputArgs`.
Position matters: `-flags` is CODEC-level (matches the `-c:v mjpeg`
context); `-fflags` is FORMAT-level and does **not** suppress this
particular segment.

ffmpeg args (S6-06):

```
-an -vf scale=800:1280 -r 12 -c:v mjpeg -q:v 6 -flags +bitexact -f mjpeg
                                                 ^^^^^^^^^^^^^^^^ S6-06 fix
```

## After — proof the marker is gone

First 48 bytes of resulting JPEG (mjpeg_bal params, same input):

```
00000000: ffd8 ffe0 0010 4a46 4946 0001 0200 0080  ......JFIF......
00000010: 002d 0000 ffdb 0043 0008 0c0c 0e0c 0e10  .-.....C........
00000020: 1010 1010 1013 1213 1414 1413 1313 1314  ................
```

APP0 segment (`ff e0`) is immediately followed by the DQT segment
(`ff db`). No `ff fe` COM marker, no `Lavc...` string. Clean.

## Cross-check across all three MJPEG profiles

All three default S6 profiles share the same OutputArgs path through
`SpecFromProfile`, so one fix lands in all three:

```
mjpeg_hq   (800x1280@10 q=4):
00000000: ffd8 ffe0 0010 4a46 4946 0001 0200 0080  ......JFIF......
00000010: 002d 0000 ffdb 0043 0008 0808 0908 090b  .-.....C........

mjpeg_bal  (800x1280@12 q=6):
00000000: ffd8 ffe0 0010 4a46 4946 0001 0200 0080  ......JFIF......
00000010: 002d 0000 ffdb 0043 0008 0c0c 0e0c 0e10  .-.....C........

mjpeg_fast (640x1024@18 q=6):
00000000: ffd8 ffe0 0010 4a46 4946 0001 0200 0080  ......JFIF......
00000010: 002d 0000 ffdb 0043 0008 0c0c 0e0c 0e10  .-.....C........
```

All three: APP0 (`ff e0`) → DQT (`ff db`). No COM (`ff fe`) segment.

## File-size delta — proves quality unchanged

| File                | Size      | Note                       |
| ------------------- | --------- | -------------------------- |
| `before.mjpeg`      | 41 939 B  | with Lavc COM marker       |
| `after.mjpeg`       | 41 921 B  | with `-flags +bitexact`    |
| **delta**           | **18 B**  | exact COM segment overhead |

18 bytes = 2 (marker `FF FE`) + 2 (length field) + 14 (`Lavc62.28.101\0`)
= COM segment overhead. No encoder-quality bytes change.

## Tests

- `TestEncodeSpec_OutputArgs_Order` — pins the exact arg layout
  including `-flags +bitexact` in its required position.
- `TestEncodeSpec_OutputArgs_HasBitexactFlag` — dedicated S6-06
  canary. Fails LOUDLY with a briefing-anchored error message if
  the flag is removed, and rejects a well-meaning rewrite to the
  format-level `-fflags` that doesn't actually fix the marker.

## Reproducing the proof locally

```sh
# Before — synthesise a JPEG with the pre-S6-06 args:
ffmpeg -hide_banner -loglevel error -f lavfi \
  -i "testsrc=duration=1:size=800x1280:rate=12" \
  -an -vf scale=800:1280 -r 12 -c:v mjpeg -q:v 6 \
  -f mjpeg -frames:v 1 -y /tmp/before.mjpeg
xxd /tmp/before.mjpeg | head -3

# After — the S6-06 args:
ffmpeg -hide_banner -loglevel error -f lavfi \
  -i "testsrc=duration=1:size=800x1280:rate=12" \
  -an -vf scale=800:1280 -r 12 -c:v mjpeg -q:v 6 \
  -flags +bitexact \
  -f mjpeg -frames:v 1 -y /tmp/after.mjpeg
xxd /tmp/after.mjpeg | head -3
```

## Scope

Affects mjpeg_hq, mjpeg_bal, mjpeg_fast. Does NOT affect:
- WebRTC / h264_passthrough (no JPEG, no COM marker)
- h264_cbp / `/stream/h264` (the H.264 path uses a different ffmpeg
  pipeline with its own arg set in `internal/h264esp`)
