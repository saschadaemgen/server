# S6-10: EncodeSpec change wasn't taking effect

## Symptom

`PUT /api/profiles/mjpeg_bal` with `fps=30, encode_quality=4`. The
registry persisted the change (visible via `GET /api/profiles`). Source
camera was delivering ~30 fps (verified via `source_fps`). RPi was not
CPU-bound (~13 %). A **freshly reconnected** ESP client (new HTTP
connection, new stats client id 35, `connected_at` after the PUT)
still reported `avg_fps ≈ 12`. The configured 30 never made it onto
the wire.

## Diagnosis

The MJPEG hub cached the encoder session under the profile name:

```go
// internal/mjpeg/hub.go (pre-S6-10)
func (h *Hub) Subscribe(name string) (*Subscriber, error) {
    h.mu.Lock()
    sess := h.sessions[name]     // ← look-up by NAME only
    if sess == nil {             // ← only read profile spec when creating
        newSess, err := h.startSessionLocked(name)
        ...
    }
    h.mu.Unlock()
    // sess.addCh ← Subscribe joins the EXISTING encoder, no spec check.
}
```

`startSessionLocked` resolves the current `EncodeSpec` from the
profile registry — **only at session-creation time**. The ffmpeg
encoder is spawned with `-r 12 -q:v 6` (the spec at that moment).
Those args are FROZEN in the running subprocess; ffmpeg has no
live-reconfigure path.

A session lives for as long as at least one subscriber is attached
to it (the briefing's bedarfsgesteuert lifecycle: last viewer gone →
session.run returns → teardown → removeSession). If a new HTTP
client arrives WHILE another viewer is still attached (parallel
connection, fast reconnect before TCP-close propagates, etc.), the
new Subscribe finds the existing session in `h.sessions[name]` and
attaches to the OLD encoder.

That matches the live measurement exactly:
- The ESP keeps a connection (or its TCP-close hadn't propagated yet)
  → old session still in map.
- A "fresh" Subscribe arrives → joins old session → old 12 fps.

Same latent bug in `internal/h264esp/hub.go::Subscribe` — `h264_cbp`
tuning would have hit the same wall.

## Fix

Every `Subscribe` now resolves the **current** Entry from the profile
registry FIRST, then compares its spec against the existing session's
spec:

```go
// internal/mjpeg/hub.go (S6-10)
func (h *Hub) Subscribe(name string) (*Subscriber, error) {
    entry, err := h.entryFor(name)    // ← read CURRENT spec from registry
    if err != nil { return nil, err }

    h.mu.Lock()
    sess := h.sessions[name]
    if sess != nil && sess.spec != entry.Spec {
        // S6-10: spec changed → retire the stale session. Its
        // existing subscribers keep streaming on the old encoder
        // until they disconnect; new ones get a fresh session.
        delete(h.sessions, name)
        sess = nil
    }
    if sess == nil {
        newSess, err := h.startSessionLockedWithEntry(name, entry)
        ...
    }
    ...
}
```

The session struct now stashes the spec it was spawned with:

```go
type session struct {
    ...
    spec EncodeSpec // S6-10
}
```

`removeSession` already guards against accidental cross-deletion via
the `want *session` pointer check, so an old session's teardown won't
clobber the new map entry.

Same change in `internal/h264esp/hub.go`.

## Lifecycle of a tuning event (after S6-10)

1. Old client connected → session `mjpeg_bal` running at fps=12.
2. `PUT /api/profiles/mjpeg_bal` with fps=30 → registry updated; the
   running encoder is unaffected (and the old client wouldn't want
   it changed mid-stream anyway).
3. New client connects:
   - `Subscribe` resolves entry → fps=30.
   - Compares against `h.sessions["mjpeg_bal"].spec` → fps=12 → mismatch.
   - Logs `mjpeg: session "mjpeg_bal" spec changed (was 800x1280@12fps q=6, now 800x1280@30fps q=4); retiring stale encoder, starting fresh`.
   - Deletes from map (old session keeps running for its existing
     subscribers).
   - Spawns a fresh encoder + session with the new spec.
   - New client attaches to fresh session at fps=30.
4. Old client eventually disconnects → old session's `run` returns →
   `teardown` → `removeSession(name, oldPointer)` finds
   `h.sessions["mjpeg_bal"]` points to the NEW session → leaves the
   map alone. Old encoder process exits cleanly.
5. Subsequent clients with no further PUT keep landing on the new
   session — fan-out invariant preserved.

## Tests

- `TestHub_SpecChangeRetiresOldSession` (mjpeg + h264esp): mutable
  resolver, two subscribes around a spec change, asserts:
  - Two separate encoders exist (not one).
  - Encoder #1 was built with the OLD spec; encoder #2 with the NEW.
  - Old subscriber still receives frames from encoder #1.
  - New subscriber receives frames from encoder #2.
  - A third subscriber lands on encoder #2 (fan-out for the new spec).
- `TestHub_SpecUnchangedJoinsExistingSession`: five subscribes,
  same spec, asserts exactly one encoder is spawned. Fan-out
  invariant.

`-race` clean.

## Verification on the live UDM

```sh
# 1. Note current avg_fps:
curl -s http://192.168.1.42:8555/stream/stats | jq '.profiles.mjpeg_bal'

# 2. Tune to 30 fps:
curl -X PUT http://192.168.1.42:8555/api/profiles/mjpeg_bal \
     -H 'Content-Type: application/json' \
     -d '{"camera_id":"679573e1...","quality":"high","usage":"esp",
          "codec":"mjpeg","width":800,"height":1280,"fps":30,"encode_quality":4}'

# 3. Reconnect the ESP (or open a fresh curl). Then:
curl -s http://192.168.1.42:8555/stream/stats | jq '.profiles.mjpeg_bal'
# avg_fps should approach 30, transcoder_cpu_percent should climb as
# the encoder now actually runs at the new rate.

# 4. Server log should show:
#    "mjpeg: session \"mjpeg_bal\" spec changed (was 800x1280@12fps q=6,
#     now 800x1280@30fps q=4); retiring stale encoder, starting fresh"
#    "mjpeg: session \"mjpeg_bal\" started (800x1280 @ 30fps q=4)"
```
