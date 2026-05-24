# S6-14: `UNIFI_ENCRYPTION` ignored — and a direction correction

## Symptom

Live RPi server started with `UNIFI_ENCRYPTION=srtp` in `.env`.
Stats showed `clients=1, avg_fps≈12` on `mjpeg_bal`, so the pipeline
was healthy — but the pull socket was using TLS (RTSPS), not SRTP.
The env was being silently overridden.

## Diagnosis (code-traced, not guessed)

The path from `?src=mjpeg_bal` to a UniFi pull goes through:

1. **`server.Server.subscribeForProfile`** builds a
   `sourcereg.Key{CameraID, Quality, Encryption}` via
   `sourceKeyFor(p)` and asks the registry to `EnsureSource(key)`.
2. **`sourcereg.Registry.EnsureSource`** uses the key to dedupe and
   calls the **factory** with that key when no source exists yet.
3. **`cmd/spike/main.go::srcFactory`** turns the key into
   `unifi.Options{Encryption: ...}` and dials.

The S6-12 implementation made `sourceKeyFor` populate
`Key.Encryption` from `p.EffectiveEncryption()`, which
canonicalizes empty-string to `"tls"`. So:

- For every profile that didn't explicitly set `encryption: "srtp"`
  (which was none of them — default-set didn't fill the field), the
  key carried `"tls"`.
- The factory in `cmd/spike` had an env-fallback for the case where
  `key.Encryption == ""`, but it never fired because the key was
  already canonicalized to `"tls"` upstream.
- Net effect: `UNIFI_ENCRYPTION=srtp` could only take effect if EVERY
  profile in storage had `encryption: "srtp"` set, which defeats the
  point of an env-level switch.

So **the env was structurally unreachable** through the per-profile
path. That's the bug.

## The direction correction (the real win)

While diagnosing, the bigger issue surfaced: **`encryption` is not a
per-profile property in the first place.** SRTP/TLS describes the
camera-side hop (UDM → server), not the per-consumer delivery side
(server → ESP / browser). Two profiles for the SAME camera can't
legally pull with different protocols simultaneously — the same RTSP
URL with `?enableSrtp` flips the entire session, not per-stream.

Putting `encryption` on a profile suggested the wrong mental model
to anyone using the API. A consumer thinking "I'll use SRTP for the
ESP profile and TLS for the browser profile against the same camera"
wouldn't get what they asked for — they'd get whichever pull
happened first, and the second profile would silently share it.

So S6-14 corrects course: **the field stays in the schema for
stability, but the steering value comes from a server-global
setting** (the env). The per-profile field becomes display-only.

## Fix — three concentric changes

### 1. Carry the global through the wiring

- `stream.ServerOptions` gains an `Encryption profile.Encryption`
  field. `NewServer` canonicalizes empty → `"tls"` and stores it on
  `Server.encryption`.
- `streambackend.Options` mirrors the same field (because the
  carvilon-side path goes through the backend wrapper, not through
  the HTTP server).
- `cmd/spike/main.go` reads `UNIFI_ENCRYPTION` from the env once at
  startup and passes the canonical value to BOTH `stream.NewServer`
  and `streambackend.New`.

### 2. Use the global in the steering key

- `sourceKeyFor` becomes a method on `Server` and pulls
  `key.Encryption` from `s.encryption`, not from
  `p.EffectiveEncryption()`. The per-profile field is no longer
  read in the steering path.
- `streambackend.toWire` follows the same rule: it builds its
  consumer-count-lookup key from `b.opts.Encryption`, not
  `p.Encryption`. (toWire and `Server.sourceKeyFor` MUST use the
  same key shape so the consumer-count hub lookup hits the same
  registry entry.)
- `cmd/spike/srcFactory` no longer needs an env-fallback because
  `key.Encryption` is now always populated by upstream. The factory
  simply does `unifi.Encryption(key.Encryption)`.

### 3. Surface the global on the wire

- `Server.handleProfiles` (GET) writes the server-global value into
  the `encryption` field for every profile, not the per-profile
  stored value. Same camera-mode for every entry — physically
  correct.
- `streambackend.toWire` does the same on the Go-level boundary
  path. Both surfaces stay consistent.
- PUT still accepts the field (so a GET→modify→PUT round-trip
  works without `DisallowUnknownFields` complaining) and still
  persists it. The persisted value is just ignored when building
  the key. It surfaces back through GET as the global value, not
  what was Put — by design.

## Why keep the field in storage and the wire schema at all?

Two reasons:

- **Wire stability.** Anyone using the S6-12 schema (admin clients,
  the carvilon-server) has already coded against the field. Ripping
  it out would cause `DisallowUnknownFields` errors on existing
  PUTs.
- **GET/PUT symmetry.** S6-09 promised that a GET array entry can
  be PUT back verbatim. That still holds — PUT now silently
  tolerates the field (it was already accepting it), GET still
  writes a meaningful value (now the global), and the round-trip
  works.

The persisted column is dead data for steering, but live data for
the wire schema. The carry-cost is one TEXT column per profile.

## Why keep `Encryption` in `sourcereg.Key`?

S6-12 added it to the key for safety: if at any point the mode
could differ within a single server instance (e.g. mid-session
flip), the registry would correctly start a separate pull rather
than splice incompatible bytes into the same hub. With S6-14 the
mode is constant within a server lifetime, so the key's encryption
field is always the same value — but the safety property is cheap
to preserve. If a future change reintroduces per-something
encryption (per-camera? per-tenant?), the key already segregates
correctly. Removing it would be an invitation to a subtle bug.

## What stayed the same

- **S6-11 SDES implementation** (`internal/source/unifi/srtp.go`).
  Untouched — when the env is `srtp`, the same code path runs.
- **S6-12 schema** (`encryption` column, `Encryption` type,
  `EncryptionTLS`/`EncryptionSRTP` constants, validator). Untouched
  on the storage side. The per-profile field still validates the
  same set of values.
- **`-flags +bitexact`** (S6-06), **`sliced-threads=0:slices=1`**
  (S6-10/-11), **fps filter chain** (S6-13). All untouched.

## Lock-down tests

- **`TestSourceKeyFor_IgnoresProfileEncryption`** — two profiles
  for the same camera, one with `encryption: "srtp"` set, one with
  `""`. Both produce the SAME key against a server with
  `Encryption: ""` (→ canonical tls). Reverses what S6-12's
  `TestSourceKeyFor_EncryptionDistinct` asserted.
- **`TestSourceKeyFor_UsesServerGlobal`** — same profile against
  two servers (one tls, one srtp) yields two distinct keys. Proves
  the global is the steering value.
- **`TestSourceKeyFor_EmptyServerEncryptionDefaultsToTLS`** —
  canonicalization end-to-end.
- **`TestProfiles_GetPut_RoundTrip`** updated: PUT stores the
  per-profile encryption, GET returns the global value, the
  persisted column reflects the global on the way back (display
  semantics).
- **`TestPutGetRoundTrip`** and
  **`TestProfile_RoundTripsTranscodedCodec`** (streambackend):
  wire-shape factories now set `Encryption: "tls"` to match what
  `toWire` returns under the default-empty-global. Round-trip
  equality holds.

`-race` clean. `GOOS=linux GOARCH=arm64` cross-compile clean. All
existing S6-11/-12/-13 tests still green.

## Live verification — what to watch for after restart

The RPi server is currently running with `UNIFI_ENCRYPTION=srtp` in
`.env`. After restart with this build:

```sh
curl -s http://192.168.1.42:8555/api/profiles | jq '.[].encryption'
# Expected: "srtp" for every profile (server-global value).

# In the server log, the source-startup line should report SDES:
#   unifi: source "<camera>/high" using SDES (SRTP) for media
# (versus the pre-fix behaviour: "using TLS (RTSPS)").
```

And, to confirm the env can go back to tls:

```sh
# Stop server, set UNIFI_ENCRYPTION=tls (or empty) in .env, restart:
curl -s http://192.168.1.42:8555/api/profiles | jq '.[].encryption'
# Expected: "tls" for every profile.
```

The per-profile encryption value in storage no longer affects what
the server does. Confirming that is the point of the new tests.

## Scope explicitly NOT done

- **Removing the `encryption` column from storage.** Reasoned
  above — wire stability and GET/PUT round-trip both prefer
  keeping it.
- **Removing the field from the wire schema.** Same reason.
- **A separate `GET /api/server` endpoint that surfaces the global
  config.** Could be added later (the global is now an interesting
  observable property); out of scope here.
- **Admin / carvilon side.** Master-chat decides whether the admin
  UI shows the encryption field as a per-profile setting (read-only
  echo of the global) or moves it to a server-config page. The
  wire shape supports either choice.
