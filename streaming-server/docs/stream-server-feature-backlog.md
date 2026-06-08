# Stream-Server Feature Backlog (CARVILON video media layer)

**Status:** Started end of Stream season 3. Living document - done items stay
(struck through context), open items carry the season they surfaced in.
Counterpart to stream-server-architecture.md (the "as built") and
stream-server-decisions.md (the "why").

> Language policy: source/docs English, chat workflow German.

---

## Done (Season 3 - cloud arc)

```
[x] ICE/STUN/TURN through CGNAT: in-process pion/turn (UDP relay + STUN on
    one port), turns: (TLS) on a public hostname. ICE reaches connected
    over the UDP relay; turns: confirmed on :5349. (D-0005)
[x] WHEP cold-start trigger: a subscriber with no publisher drives a
    request_publish to the edge via an Open-Core callback. (D-0007)
[x] Media path end-to-end provable via cmd/whep-probe (real pion WHEP
    subscriber with its own ICEServers).
[x] TURN/ICE telemetry data source: TURNStats getter + OnTURNEvent +
    whipclient OnICEState (Open-Core types). (D-0006)
[x] Egress auth: WHEP egress-token verify (separate HMAC key, fail-closed,
    401 before the cold-trigger). (D-0008)
```

## Open (Season 4 / Master-19 and beyond)

```
[ ] Subscriber ICEServers signalling for Android (the gating item). ICEServers
    do NOT travel in the SDP (D-0005), so a NAT client must be given its own.
    Chosen path (ii): the Master hands Android stun/turn/turns + short-lived
    REST creds at stream start (the symmetric counterpart of the edge's
    request_publish ICEServers). Alternative (i): WHEP Link: rel="ice-server"
    header (RFC 9725) + an OPTIONS preflight - more standard, heavier client.
[ ] Public WHEP URL over a hostname (so a browser/Android trusts the TLS
    without the cloudca CA). Today :8444 uses the private cloudca cert.
[ ] Android SDP/offer briefing: confirm POST /whep/{sid} (application/sdp,
    Bearer egress token) is the agreed client contract.
[ ] JWS-to-asymmetric token swap (jointly with publish_token; shared crypto
    owned centrally - secure-chat-wire-format.md).
[ ] Egress hardening: per-client token binding (today sid-bound, ~5 min) and
    egress rate-limiting. (stream-server-security.md "open debt")
[ ] IPv6 STUN noise (cosmetic): mDNS/v6 host candidates in the debug log.
[ ] icedebug: optional RelatedAddress enrichment (srflx/relay base) - cosmetic
    diagnostics only.
```

## Carried from earlier seasons (still relevant)

```
[ ] Three-tier source profiles (high/mid/low switchable; Quality already in
    the pull key). MJPEG could pull the mid tier for more headroom.
[ ] h264esp/encoder.go still on wallclock + default scaler (unused; symmetric
    S3-01 fix if ever used - D-0003).
[ ] systemd service on the RPi (edge runs as a user service today).
[ ] Merge the stream binary into the carvilon binary (GOPRIVATE) - deferred
    while both products develop separately (Open-Core via build tags holds).
[ ] Two-way audio, sales features (watermark / event recording / live-grid),
    multi-camera, license/tenant limits (concept - see season index).
```

---

*Living document. Last: end of Stream season 3.*
