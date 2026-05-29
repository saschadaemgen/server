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

---

## Verify (stats + profiles)

```bash
curl -s http://localhost:8555/stream/stats     # clients (all codecs), fps, CPU
curl -s http://localhost:8555/api/profiles      # array of profiles (snake_case)
```

After the S6-13 fps fix: `encoder input channel full` must be GONE,
frames_dropped=0, even avg_fps. After S6-15: WebRTC viewers appear in stats
and disappear on tab close (within 30s worst case via the idle watchdog).

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

*Living document. Last: 2026-05-25 (end of Stream season 1, RPi deploy +
env vars + srtp mode).*
