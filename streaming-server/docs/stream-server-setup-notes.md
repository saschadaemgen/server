# Stream-Server Setup Notes (CARVILON video media layer)

Operational steps for building, deploying and running the CARVILON stream
server. Growing list; extend when new steps are needed. Counterpart to the
server-side setup-notes.md (master chat) and esp-setup-notes.md (ESP chat).

**Repo:** `C:\Projects\UniFi\streaming-server`
**Build machine:** Windows desktop (PowerShell), cross-compile to RPi arm64
**Stream host:** Raspberry Pi 192.168.1.42 (ssh alias `rpi`, user sash710)

> Language policy: all source, comments and docs are English. Chat workflow
> German (JARVIS style).

---

## Build (cross-compile for the RPi)

**When:** before every RPi deploy.

```powershell
cd C:\Projects\UniFi\streaming-server
$env:GOOS="linux"; $env:GOARCH="arm64"
go build -o carvilon-stream-rpi ./cmd/streaming-server
$env:GOOS=""; $env:GOARCH=""        # IMPORTANT: reset afterwards
```

Local test build (Windows, no cross-compile): `go build ./cmd/streaming-server` and run
locally - note the camera is on the LAN, so the desktop must reach the UDM.

---

## Deploy + run (RPi)

**When:** after every build, to test on the real ESP/browser.

```bash
# on the RPi (ssh rpi):
pkill -f carvilon-stream-rpi          # else scp fails: "dest open failure"
# from the desktop:
#   scp carvilon-stream-rpi rpi:~/
# on the RPi, start (tls default):
UNIFI_NVR_HOST=192.168.1.1 UNIFI_API_KEY=<key> ./carvilon-stream-rpi
# or srtp mode:
UNIFI_NVR_HOST=192.168.1.1 UNIFI_API_KEY=<key> UNIFI_ENCRYPTION=srtp ./carvilon-stream-rpi
```

The server runs in the FOREGROUND and dies when the SSH window closes. A
systemd service is an open TODO. In a pure LAN, tls is sufficient; srtp is for
external/cloud access (and adds harmless RTCP log noise).

Healthy startup log markers:

```
store: opened ./state/stream.db
backend: N profile(s) loaded from DB
stream: signaling + test page on http://[::]:8555
sourcereg: hub created for <cam>:high/<mode> (idle until first subscriber)
unifi: encryption mode=<tls|srtp>       (must match the env!)
unifi: SRTP receiver armed              (only in srtp mode)
hub: source started for new subscriber  (on first consumer)
```

---

## Environment variables

```
UNIFI_NVR_HOST     UDM address (192.168.1.1)
UNIFI_API_KEY      Protect integration API key (X-API-KEY) - NEVER log/commit
UNIFI_ENCRYPTION   tls (default) | srtp - GLOBAL source encryption (S6-14)
```

State: `./state/stream.db` (SQLite profile persistence). Survives restarts;
the built-in default profile set is only seeded when the DB is empty (DB wins).

### Cloud role env (Season 3) - names/purpose/format ONLY, never values

```
# read by `cmd/streaming-server -role=cloud` (the repo dev cloud server):
CARVILON_WHIP_LISTEN            :8444 (default). WHIP ingress + WHEP egress.
CARVILON_WHIP_CERT_FILE/_KEY    server TLS cert/key (private cloudca CA).
CARVILON_PUBLISH_TOKEN_HMAC_KEY 32-byte hex. WHIP publish-token verify key.
CARVILON_EGRESS_TOKEN_HMAC_KEY  32-byte hex. WHEP egress-token verify key.
                                SEPARATE from the publish key. Empty -> the
                                WHEP egress FAILS CLOSED (every subscribe 401).

# read by the carvilon-server (Master cfg) and passed via CloudSetupOptions
# (the stream package never reads these directly):
CARVILON_TURN_PUBLIC_IP         public IP the relay announces (stun/turn).
CARVILON_TURN_SHARED_SECRET     HMAC shared secret for ephemeral TURN creds.
CARVILON_TURN_REALM             TURN realm (default "carvilon").
CARVILON_TURN_UDP_PORT          UDP relay + STUN port (default 3478).
CARVILON_TURN_TLS_PORT          turns: port (e.g. 5349). 0 -> TLS leg OFF.
CARVILON_TURN_PUBLIC_HOST       public hostname for turns: (cert SAN must match).
CARVILON_TURN_TLS_CERT_FILE/_KEY  publicly-trusted cert for the turns: leg,
                                SEPARATE from the WHIP cloudca cert; empty ->
                                falls back to the WHIP cert. Example path:
                                /etc/letsencrypt/live/<turn-host>/fullchain.pem.
```
The HMAC keys and the TURN shared secret are SECRETS - never log/commit/echo
a value (the code logs only the env NAME + byte length). See
stream-server-security.md.

### Test helpers (NOT in the production build)

Two standalone `cmd/` diagnostics, each its own `package main`; the edge/cloud
binaries never import them. Run with `go run` (the RPi has no `go`; the desktop
does):

```
cmd/whep-probe   - a real pion WHEP subscriber: builds an offer, POSTs to
                   /whep/{sid}, applies the answer, logs HTTP status + ICE
                   state + RTP arrival. Flags: -url, -token (Bearer egress
                   token), -stun/-turn (+ -turn-user/-turn-pass), -hold,
                   -insecure (default true; :8444 uses the cloudca cert).
cmd/mint-egress  - mints a token in the publish/egress format. Flags: -sid,
                   -ttl, -key-env (default CARVILON_EGRESS_TOKEN_HMAC_KEY; set
                   CARVILON_PUBLISH_TOKEN_HMAC_KEY for the key-separation test).
                   Key ONLY from the env, never logged; stdout = ONLY the token.
```
Three-case egress test: no -token -> 401; valid egress token -> 201 + RTP;
publish-key token -> 401 (key separation).

---

## Verify (stats + profiles)

```bash
curl -s http://localhost:8555/stream/stats     # clients (all codecs), fps, CPU
curl -s http://localhost:8555/api/profiles      # array of profiles (snake_case)
```

After the S6-13 fps fix: `encoder input channel full` must be GONE,
frames_dropped=0, even avg_fps. After S6-15: WebRTC viewers appear in stats
and disappear on tab close (within 30s worst case via the idle watchdog).
After S2-16 (low_delay removed): on a heavy 1200x1600 source, ffmpeg CPU drops
to single digits and `encoder input` drops vanish over many minutes of motion.
If `encoder input` drops reappear, do NOT re-add low_delay - check
stream-server-decisions.md D-0002 first.

---

## Recovery: back to solid ground

**When:** a rebuild breaks behavior.

```bash
git log --oneline -15
git reset --hard <good-commit>
git reflog            # removed commits are still reachable here
```

---

## Git workflow

```
Claude Code commits LOCALLY (Conventional Commits), never pushes.
Sascha pushes manually after milestone / season end.
   cd C:\Projects\UniFi\streaming-server
   git push
.gitignore includes /carvilon-stream-rpi (arm64 build artifact) + state/.
Work directly on main (no worktrees).
Before each season push: are CLAUDE.md + docs/ (architecture, wire-format,
security, profile-api) current?
```

---

## Hard rules (anchors)

```
- MIT/Apache dependencies only. NEVER AGPL (go2rtc + mediamtx excluded).
- stdlib default; ask "could stdlib do this?" before any new dep.
- Never change version numbers in config files without approval.
- Token / API key / RTSPS URL never logged or committed.
- MEASURE RULE: before any test, ask which server runs, on which host/port,
  fresh or old. Several instances may run; never assume one is stopped.
- For latency: always compare against a working reference (go2rtc), don't
  blame the method.
```

---

*Living document. Last: end of Stream season 3 (cloud env + test helpers).
Siblings: stream-server-decisions.md (WHY / learnings) and
stream-server-security.md (the layer's security view).*
