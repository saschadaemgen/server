# Stream-Server wire format (media-layer side)

**Status:** Updated end of Stream season 1 (25 May 2026). Living document.
Counterpart to the carvilon-server wire-format.md (master chat) and
esp-wire-format.md (ESP chat). THIS is the authoritative stream-server view;
reconcile the others on changes.
**Server:** stream server on RPi `192.168.1.42:8555` (MJPEG auth-free; WebRTC
signalling proxied by carvilon-server :9080).

> Language policy: docs English. Chat workflow German (JARVIS style).

## 0. Encryption is global (S6-14)

```
Source encryption (tls/srtp) is set globally via UNIFI_ENCRYPTION env, NOT per
request and NOT per profile. The per-profile `encryption` field in the API is
DISPLAY-ONLY (mirrors the active global mode). It is accepted on PUT for schema
stability but does NOT steer the pull.
```

## 1. MJPEG stream (ESP + simple consumers)

```
GET http://192.168.1.42:8555/api/stream.mjpeg?src=<profile>   (auth-free)
Body: multipart/x-mixed-replace; boundary=frame
      per part: JPEG (e.g. mjpeg_bal delivers 800x1280)
Server applies -flags +bitexact (no COM marker; else the P4 HW decoder fails).
Health: GET http://192.168.1.42:8555/api/profiles
```

## 2. WebRTC (browser / WebViewer)

```
POST /offer        (on the stream server)
   carvilon-server proxies the browser's POST /webviewer/offer to this.
   Body: SDP offer. Response: SDP answer.
   Codec: h264 passthrough (no transcode). Profile resolved as intercom_web
   (h264_passthrough) by default.
Media: after signalling, the stream runs over the WebRTC peer connection
   (ICE/DTLS-SRTP) - DTLS-SRTP is WebRTC's own encryption, independent of the
   source encryption setting.
```

## 3. H.264-CBP transcode

```
GET /stream/h264?src=<profile>
   Constrained Baseline transcode path (for consumers needing CBP).
```

## 4. Profile API (11-field schema, snake_case)

```
GET    /api/profiles            -> JSON ARRAY (NOT a go2rtc-style map)
GET    /api/profiles/{name}     (single; note: master chat filters from the
                                 array, there was no reliable single-GET early)
PUT    /api/profiles/{name}     -> create/update (DisallowUnknownFields)
DELETE /api/profiles/{name}

Schema (all 11 fields, snake_case; GET == PUT shape):
   name            string
   camera_id       string
   quality         string ("high")
   usage           string ("esp" | "browser")
   description     string
   codec           string ("mjpeg" | "h264_passthrough" | "h264_cbp")
   width           int    (0 for passthrough)
   height          int    (0 for passthrough)
   fps             int    (0 for passthrough)
   encode_quality  int    (q:v for mjpeg; CRF for cbp)
   encryption      string ("tls" | "srtp") - DISPLAY-ONLY, reflects global mode

Default set (seeded only when DB empty):
   h264_cbp       (esp, 800x1280, 15fps, CRF 26, h264_cbp)
   intercom_web   (browser, h264_passthrough)
   mjpeg_bal      (esp, 800x1280, 12fps, q:v6)   <- ESP default
   mjpeg_fast     (esp, 640x1024, 18fps, q:v6)
   mjpeg_hq       (esp, 800x1280, 10fps, q:v4)
```

## 5. Stats API

```
GET /stream/stats
{
  "generated_at": "<rfc3339>",
  "global": { "clients": <int, all codecs>, "frames_sent_total": ...,
              "bytes_sent_total": ..., "transcoder_cpu_percent": <float> },
  "profiles": {
    "<name>": { "profile","codec","clients","frames_sent","frames_dropped",
                "bytes_sent","avg_fps","avg_bitrate_kbps",
                "source_frames","source_fps" }
  },
  "clients": [
    { "id","profile","codec","remote_addr","connected_at","uptime_sec",
      "frames_sent","frames_dropped","bytes_sent","avg_fps",
      "avg_bitrate_kbps","last_frame_at" }
  ]
}
   codec for WebRTC clients = "h264_passthrough" (NOT "webrtc" - that would
   split the per-profile aggregation). WebRTC clients appear since S6-15 and
   are removed on teardown / after a 30s idle watchdog.
   The master chat's admin consumer column reads profiles.<name>.clients.
```

## 6. Source-side SDP (SRTP/SDES observation)

```
On ?enableSrtp the UA Intercom SDP carries SDES (RFC 4568), NOT MIKEY:
   a=crypto count=3, suite AES_CM_128_HMAC_SHA1_80, inline = 30 bytes
                     (16 key + 14 salt), in CLEARTEXT
   a=key-mgmt count=0   (no MIKEY)
   media.Profile=AVP, session/media KeyMgmtMikey=false
tls mode: a=crypto count=0 (plain RTP in the TLS tunnel).
The H.264 track is declared packetization-mode 1 but sends mode-2 packets
(own depacketizer handles this). Video track only; audio tracks present but
not pulled.
```

## 7. carvilon-server seam (StreamBackend)

```
Steering (CRUD / ListCameras) goes through the in-process StreamBackend
interface in the commercial build (the carvilon-server wraps it). The HTTP
endpoints above are the dev/standalone view. Reconcile with the carvilon
wire-format.md when the seam changes.
```

## 8. Cloud signalling (WHIP/WHEP, ICEServers, egress token) - Season 3

```
WHIP ingress  POST /whip/{streamID}   (edge -> cloud, the publisher)
  Authorization: Bearer <publish_token>   (verified, separate from egress)
  Content-Type: application/sdp           Body: raw SDP offer
  201 -> SDP answer in body + Location: /whip/{streamID}/session/<id>
  401 bare on any auth failure (detail logged server-side only).

WHEP egress   POST /whep/{streamID}   (subscriber -> cloud)
  Authorization: Bearer <egress_token>    (REQUIRED; S3 egress-auth)
  Content-Type: application/sdp           Body: raw SDP offer
  201 -> SDP answer + Location: /whep/{streamID}/session/<id>
  401 bare (no/invalid/expired/wrong-sid token, or fail-closed no-key)
  404 no publisher AND no cold-trigger wired
  503 publisher session exists but track not ready in time
  504 cold trigger ran but no publisher docked / no edge (NOT 404)
```

**ICEServers (minted per peer, served in the PeerConnection config - NOT in
the SDP):** a credential-less STUN entry `stun:<publicIP>:<udpPort>`, a TURN
entry `turn:<publicIP>:<udpPort>?transport=udp` with ephemeral REST creds,
and (when a TLS port + public host are set) a turns entry
`turns:<host>:<tlsPort>?transport=tcp`. The edge receives the same shape in
the side-channel `request_publish` frame. IMPORTANT: ICEServers are a CLIENT
config, they do NOT travel in the SDP - a NAT subscriber must be given its own
(D-0005).

**Egress token format:** byte-identical to the publish token
(`base64url(json{sid,exp,nonce}).base64url(HMAC-SHA256(key, payloadString))`),
signed with a SEPARATE key. The shared token/crypto format is owned centrally
- see **secure-chat-wire-format.md**; it is NOT duplicated here. This doc only
notes that the egress uses the same format with its own key (D-0008).

---

*Living document. Last: end of Stream season 3 (cloud signalling: WHIP/WHEP,
ICEServers, egress token - Section 8). For the shared token/crypto format see
secure-chat-wire-format.md. Reconcile with carvilon wire-format.md and
esp-wire-format.md on changes. Sibling: stream-server-decisions.md.*
