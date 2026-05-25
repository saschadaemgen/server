# S6-15: WebRTC viewers missing from `/stream/stats`

## Symptom

A WebRTC viewer (intercom_web in the browser) was visibly streaming
H.264 over `/offer`, but did not appear anywhere in `/stream/stats`:

```
"global":   { "clients": 1 }              ← should be 2
"profiles": { "mjpeg_bal": { "clients":1 } }   ← only MJPEG
"clients":  [ { "id":18, "codec":"mjpeg", ... } ]
```

No entry in the global count, the per-profile block, or the
client list.

## Diagnosis (code-traced)

`/stream/stats` is fed by `stats.Registry`. Every viewer endpoint
that goes through the **HTTP write boundary** registers a
`*stats.Client` for the lifetime of the response:

- `handleMJPEG` (`server.go:643-669`): `Register(profileName,
  codec, RemoteAddr)` before the write loop, `defer Unregister`,
  `RecordFrame(n)` per successful write, `RecordDrop()` on err.
- `handleH264` (`server.go:744-766`): same pattern.

`handleOffer` (`server.go:443-563`) does NOT use that pattern.
The HTTP response only carries the SDP **answer**; the media
moves over the WebRTC peer connection's separate ICE/DTLS/SRTP
sockets. The actual streaming happens inside a goroutine launched
at line 530 that calls `feedTrack` to pump access units from a
`hub.Subscriber` into a `webrtc.TrackLocalStaticSample`.

That goroutine was never registering anything with `stats.Registry`
— a pure instrumentation gap. The stream worked; it was just
invisible.

## Fix

Two surgical changes, no schema churn, no MJPEG-path touch.

### 1. Register the client inside the WebRTC goroutine

```go
go func() {
    var sc *stats.Client
    if s.stats != nil {
        sc = s.stats.Register(p.Name, string(p.Codec), r.RemoteAddr)
        defer s.stats.Unregister(sc)
    }
    defer func() { _ = pc.Close() }()
    s.feedTrack(sub, track, feedDrops, sc)
}()
```

- **Codec** uses `string(p.Codec)` (= `"h264_passthrough"` for /offer
  profiles), matching the convention `handleMJPEG` / `handleH264`
  follow. Per-profile aggregation in `stats.Snapshot` stays
  consistent — a profile's clients all carry the same codec
  string.
- **Defer order is LIFO**: `pc.Close()` runs FIRST (added second,
  popped first), `Unregister` runs LAST. The wire is gone before
  the snapshot loses the entry — no race where a stats poll
  shows "0 clients" while DTLS-SRTP packets are still in flight.

### 2. `feedTrack` records frames + has a defensive idle timeout

```go
func (s *Server) feedTrack(sub *hub.Subscriber,
                           track *webrtc.TrackLocalStaticSample,
                           drops *droplog.Counter,
                           sc *stats.Client) {
    // ...
    timer := time.NewTimer(webrtcIdleTimeout)
    defer timer.Stop()
    for {
        select {
        case au, ok := <-sub.Frames():
            if !ok { return }
            // reset timer ...
            payload := annexBMarshal(au.NALUs)
            if err := track.WriteSample(...); err != nil {
                drops.Record(err)
                sc.RecordDrop()
                continue
            }
            sc.RecordFrame(len(payload))
        case <-timer.C:
            // No frame for webrtcIdleTimeout. Returning here lets
            // the caller's deferred Unregister + pc.Close run.
            return
        }
    }
}
```

- `RecordFrame(len(payload))` counts the H.264 AU size handed
  to pion (the same pre-wire semantic MJPEG uses: bytes written
  into the delivery layer, not bytes on the network after RTP+
  SRTP framing).
- `RecordDrop()` is called when pion's `WriteSample` returns an
  error — symmetric with `handleMJPEG`'s drop accounting.
- `*stats.Client` is **nil-safe**: `RecordFrame` / `RecordDrop`
  return early on nil. The "no stats registry" path is identical
  to MJPEG.

### Why the idle timeout matters (the lifecycle pflicht)

MJPEG gets teardown for free: the HTTP write fails when the
client disconnects → the handler returns → `defer Unregister`
runs. WebRTC doesn't get that. The teardown trigger is
`pc.OnConnectionStateChange` firing with `Failed/Closed/
Disconnected`, which calls `sub.Close()` → `sub.Frames()` closes
→ `for au := range` exits → goroutine's defer chain runs.

Two failure modes that path doesn't survive:

1. **pion never fires the state change** (network blackhole,
   bug, pathological NAT behavior). The subscriber stays open,
   `sub.Frames()` stays blocked, goroutine never exits,
   stats client never deregisters → **ghost** in /stream/stats.
2. **Upstream camera dies but pion thinks the viewer is fine**
   (e.g. STUN keepalives still succeed against an empty stream).
   Same shape: stats client lingers.

The fix: a 30-second idle timer in `feedTrack`. If no AU has
been received for that window, the timer fires, `feedTrack`
returns, defer chain runs, ghost is gone. 30 s is the most
conservative healthy-stream threshold — even the slowest
configured profile (mjpeg_hq @ 10 fps = 100 ms inter-frame)
sees the timer reset 300 times within the window. `/offer`
profiles see 30 fps from the camera, which is 900 resets per
window.

The variable is a package-level `var` (not `const`) so unit
tests can shrink it to 50–80 ms without compile-time gymnastics.

## What stayed the same

- **MJPEG / H264-CBP HTTP handlers** — untouched.
- **`stats.Registry` API** — no new methods, no new fields,
  no JSON-shape change.
- **`OnConnectionStateChange` callback** — still triggers
  `sub.Close()` + `pc.Close()`; that's still the primary
  teardown path. The idle timeout is the defensive secondary.
- **`/offer` SDP negotiation path** — bytes through the
  signalling channel are unchanged. The browser does not need
  to know stats now tracks it.

## Lock-down tests (`server_webrtc_stats_test.go`)

A self-contained `statsFakeSource` + a real `hub.Hub` + a real
unbound `webrtc.TrackLocalStaticSample` (its `WriteSample`
returns nil early when no peer is bound, perfect for unit-level
testing of the feed loop):

- **`TestFeedTrack_RecordsFramesIntoStatsClient`** — push three
  AUs, assert `FramesSent==3`, `BytesSent==21` (7 bytes per
  Annex-B-marshalled NAL × 3), profile aggregation correct,
  codec field is "h264_passthrough" (NOT "webrtc").
- **`TestFeedTrack_ReturnsOnSubscriberClose`** — close the
  subscriber, assert feedTrack exits within 1 s. (Primary
  teardown path.)
- **`TestFeedTrack_ReturnsOnIdleTimeout`** — patch
  `webrtcIdleTimeout` to 50 ms, push nothing, assert exit
  within 2 s. (Defensive teardown path.)
- **`TestFeedTrack_IdleTimerResetsOnFrames`** — patch idle
  timeout to 80 ms, push every 30 ms for 200 ms, assert
  feedTrack does NOT exit during streaming, then stops feeding
  and asserts exit within 500 ms. (Timer reset logic is
  correct.)
- **`TestFeedTrack_NilStatsClientIsSafe`** — pass nil
  `*stats.Client`, push an AU, assert no panic.

`-race` clean. `GOOS=linux GOARCH=arm64` cross-compile clean.

## Live verification (Mess-Regel: VOR der Messung Sascha fragen)

The RPi server's state must be confirmed before measuring:

- **Is the server running?** Which host/port? Fresh build with
  this commit, or the old binary?
- After restart with this build:
  ```sh
  # One WebViewer in the browser + one MJPEG client at the same time.
  curl -s http://192.168.1.42:8555/stream/stats | jq '.global.clients'
  # Expected: 2

  curl -s http://192.168.1.42:8555/stream/stats | jq '.clients'
  # Expected: two entries, one with codec="mjpeg", one with
  # codec="h264_passthrough" (the WebRTC viewer of intercom_web).
  ```
- Close the browser tab, wait ~1 s (state change), poll again:
  ```sh
  # Expected: WebRTC entry is gone. Global.Clients drops to 1.
  ```
- Worst-case sanity: if for some reason `pc.OnConnectionStateChange`
  never fires (test by killing the WiFi to the browser host
  abruptly), the entry should still disappear within ~30 s
  (idle timeout). The MJPEG client must be unaffected.

## Scope explicitly NOT done

- **No new stats schema fields.** Existing
  `ClientSnapshot` / `ProfileSnapshot` shapes are reused
  verbatim.
- **No MJPEG-path change.** Briefing pinned.
- **No admin-side change.** Once `/stream/stats` reports
  correctly, the admin UI's consumer column updates
  automatically. If it doesn't, that's a separate field-
  mapping issue and belongs in the master chat.
- **No exposed knob** for `webrtcIdleTimeout`. The 30 s
  default is conservative enough; if it ever needs tuning,
  promote to a `ServerOptions` field later.
