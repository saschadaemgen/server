# Stream-Server HTTP API — S6-08 inventory for carvilon-admin docking

Status: **factual inventory** of what the server speaks today (commit `74939be` +
working tree). No code changes recommended in this doc — only the truth, so
the carvilon-side admin can be plugged in correctly.

Server listens on `CARVILON_STREAM_LISTEN` (default `:8555`).

---

## 1. Profile endpoints

### 1.1 GET `/api/profiles` — list all profiles

- **Method:** `GET` only. `405 Method Not Allowed` for everything else.
- **Auth:** none today (the spike runs on `127.0.0.1`; auth lives at the
  carvilon-server proxy layer).
- **Query params:** none.
- **Response:** `Content-Type: application/json`, `Cache-Control: no-cache`,
  status `200`. Body is a JSON **array** of profile objects, ordered
  alphabetically by `name`.

**Field names — camelCase** (this is the live `handleProfiles` Fprintf format
in `server.go`):

| field         | type   | notes                                                  |
| ------------- | ------ | ------------------------------------------------------ |
| `name`        | string | client `?src=` key                                     |
| `cameraID`    | string | UniFi Protect camera identifier (**camelCase**)        |
| `quality`     | string | `high` / `medium` / `low`                              |
| `usage`       | string | `browser` / `esp`                                      |
| `description` | string | admin UI label, may be empty                           |
| `codec`       | string | `h264_passthrough` / `mjpeg` / `h264_cbp`              |
| `width`       | number | output pixels (0 for `h264_passthrough`)               |
| `height`      | number | output pixels (0 for `h264_passthrough`)               |
| `fps`         | number | target frame rate (0 for `h264_passthrough`)           |
| `encodeQuality` | number | mjpeg q:v / h264 CRF (0 for `h264_passthrough`, **camelCase**) |

**Live sample** (the spike's S6-03 default-set on the intercom; reproduced
byte-for-byte by `go run ./cmd/gen-docs` → `docs/api-sample-profiles-list.json`):

```json
[
  {"name":"h264_cbp","cameraID":"679573e101080b03e4000424","quality":"high","usage":"esp","description":"ESP: H.264 Constrained Baseline, 800x1280 @ 15 fps, CRF 26","codec":"h264_cbp","width":800,"height":1280,"fps":15,"encodeQuality":26},
  {"name":"intercom_web","cameraID":"679573e101080b03e4000424","quality":"high","usage":"browser","description":"Intercom (browser reference, H.264 passthrough via WebRTC)","codec":"h264_passthrough","width":0,"height":0,"fps":0,"encodeQuality":0},
  {"name":"mjpeg_bal","cameraID":"679573e101080b03e4000424","quality":"high","usage":"esp","description":"ESP: MJPEG, 800x1280 @ 12 fps, q:v 6 (balanced)","codec":"mjpeg","width":800,"height":1280,"fps":12,"encodeQuality":6},
  {"name":"mjpeg_fast","cameraID":"679573e101080b03e4000424","quality":"high","usage":"esp","description":"ESP: MJPEG, 640x1024 @ 18 fps, q:v 6 (fast)","codec":"mjpeg","width":640,"height":1024,"fps":18,"encodeQuality":6},
  {"name":"mjpeg_hq","cameraID":"679573e101080b03e4000424","quality":"high","usage":"esp","description":"ESP: MJPEG, 800x1280 @ 10 fps, q:v 4 (high quality)","codec":"mjpeg","width":800,"height":1280,"fps":10,"encodeQuality":4}
]
```

### 1.2 Single-profile GET — **does NOT exist**

There is no `GET /api/profiles/{name}`. Clients filter the array from the
list endpoint themselves. If you hit `GET /api/profiles/intercom_web`, the
mux routes nothing for it and falls through to the embedded file server
(probably a 404 from `web/`).

### 1.3 PUT `/api/profiles/{name}` — upsert one profile

- **Method:** `PUT`. The mux pattern is literally `"PUT /api/profiles/{name}"`
  (Go 1.22 method+path routing). `405` for anything else on that path.
- **Body:** `application/json` (server reads up to 64 KiB, calls
  `dec.DisallowUnknownFields()` — typos or extra keys cause `400`).
- **Name resolution:** URL wins; a `"name"` key in the body would be
  rejected by `DisallowUnknownFields`. There is NO `"name"` field in the
  PUT body shape.
- **Persistence:** writes to SQLite (`profile.PutProfile`) AND refreshes
  the in-memory `profile.Registry`. New `?src=` lookups see the change
  immediately; existing viewers stay on their current hub until they
  disconnect.

**Body fields — snake_case** (this is `profileJSON` in `server.go`):

| field            | type   | required           | notes                                  |
| ---------------- | ------ | ------------------ | -------------------------------------- |
| `camera_id`      | string | yes                | **snake_case** (cf. list's `cameraID`) |
| `quality`        | string | yes                | `high` / `medium` / `low`              |
| `usage`          | string | yes                | `browser` / `esp`                      |
| `description`    | string | no                 | empty string allowed                   |
| `codec`          | string | yes                | `h264_passthrough` / `mjpeg` / `h264_cbp` |
| `width`          | number | iff codec≠passthrough | 1..8192                              |
| `height`         | number | iff codec≠passthrough | 1..8192                              |
| `fps`            | number | iff codec≠passthrough | 1..60                                |
| `encode_quality` | number | iff codec≠passthrough | 1..51 (**snake_case**)               |

**Responses:**

- `204 No Content` — success.
- `400 Bad Request` — malformed JSON, unknown field, validation failure
  (returns the validator's error message in body).
- `404` — not produced by PUT (upsert pattern).
- `503 Service Unavailable` — server built without a `ProfileWriter`
  (cmd/spike always wires one, so 503 on PUT means the carvilon side
  forgot to enable it).
- `500` — store / registry sync error (rare).

**Example:**

```sh
curl -X PUT http://127.0.0.1:8555/api/profiles/mjpeg_bal \
     -H 'Content-Type: application/json' \
     -d '{
       "camera_id":   "679573e101080b03e4000424",
       "quality":     "high",
       "usage":       "esp",
       "description": "ESP: MJPEG, balanced (tuned)",
       "codec":       "mjpeg",
       "width":       800,
       "height":      1280,
       "fps":         15,
       "encode_quality": 5
     }'
```

### 1.4 DELETE `/api/profiles/{name}` — remove one profile

- **Method:** `DELETE`. Pattern `"DELETE /api/profiles/{name}"`.
- **Body:** none.
- **Effect:** removes the profile from store + registry. Live viewers of
  that profile keep streaming until they disconnect; new `?src=`
  lookups for the deleted name 404.

**Responses:**

- `204 No Content` — deleted.
- `404 Not Found` — body `"unknown profile"` (sentinel: handler does
  `errors.Is(err, profile.ErrUnknownProfile)` plus a substring sniff
  for `"not found"` / `"unknown"`).
- `503` — no `ProfileWriter` configured.

### 1.5 POST to create — **does NOT exist**

There is no `POST /api/profiles`. `PUT /api/profiles/{name}` is an
**upsert** — call it with a name that doesn't exist yet and it creates the
profile. The carvilon-admin's "create new" UX must therefore PUT with the
new name in the URL.

---

## 2. WebRTC signaling — POST `/offer`

- **Method:** `POST` only. `405` for everything else.
- **Query:** `?src=<profile_name>` — required; `400` if missing.
- **Body:** raw SDP offer text, read up to 1 MiB. Server does NOT enforce
  a Content-Type, but the browser test page sends `application/sdp`.
  `400` if body is empty.
- **Codec gate:** only profiles with `codec=h264_passthrough` are served
  via `/offer`. For mjpeg / h264_cbp profiles the server returns
  `400 Bad Request` with a message that names the right endpoint
  (`/api/stream.mjpeg` / `/stream/h264`).
- **Flow:** read SDP offer → subscribe to the camera hub (which lazy-
  starts the RTSPS pull if this is the first viewer for that
  `(CameraID, Quality)` combo) → build a per-viewer pion
  `TrackLocalStaticSample` H.264 track → ICE gathering → return the SDP
  answer.
- **Response:** `Content-Type: application/sdp`, status `200`. Body is the
  full local SDP including ICE candidates.
- **Lifecycle:** on `PeerConnectionStateFailed / Closed / Disconnected`
  the server unsubscribes from the camera hub. Last subscriber gone ⇒
  RTSP pull stops.

**Error codes:**

- `400` — missing `?src=`, empty body, wrong codec on profile, bad SDP.
- `404` — unknown profile (`profile.ErrUnknownProfile`).
- `500` — pion track / peer construction failed.
- `503` — upstream RTSPS subscribe failed.

**Verifying live:** the spike's `web/index.html` posts to `/offer` and
loads the answer into a pion-compatible JS client. If the dropdown shows
`intercom_web` and clicking **Connect** results in a playing video, then
`/offer` is healthy.

---

## 3. Stream endpoints (reference; carvilon-admin only proxies, doesn't decode)

These are NOT used by the admin UI directly — the carvilon-server proxies
them through to the viewer. Listed here for completeness.

| Method + Path                       | Codec gate         | Output                                 |
| ----------------------------------- | ------------------ | -------------------------------------- |
| `GET /api/stream.mjpeg?src=<name>`  | `mjpeg`            | `multipart/x-mixed-replace; boundary=frame` (see `docs/mjpeg-wire-format.md`) |
| `GET /stream/h264?src=<name>`       | `h264_cbp`         | `video/h264` chunked Annex-B (S6-02)   |

Also informational:

| Method + Path        | Purpose                                                       |
| -------------------- | ------------------------------------------------------------- |
| `GET /stream/stats`  | JSON throughput snapshot (`docs/stats-sample-*.json`)         |
| `GET /healthz`       | `204` liveness                                                |

---

## 4. Known inconsistencies (carvilon-admin will trip on these)

### 4.1 LIST returns camelCase, PUT expects snake_case

| Field on the wire | GET `/api/profiles`     | PUT body                |
| ----------------- | ----------------------- | ----------------------- |
| Camera identifier | `cameraID`              | `camera_id`             |
| Encoder quality   | `encodeQuality`         | `encode_quality`        |
| (others)          | lowercase               | lowercase               |

A naïve round-trip — fetch profile X via GET, change one field, send it
back via PUT — **does not work** out of the box. PUT's
`DisallowUnknownFields` rejects `cameraID` and `encodeQuality` with a
`400`. The admin client must rename two keys.

Original commit history: the GET handler hand-rolls JSON with camelCase
keys (server.go), the PUT handler defines a separate `profileJSON` struct
with snake_case tags. Both predate this briefing; not changed here per
scope (§6 of the briefing).

### 4.2 No HTTP create endpoint

`POST /api/profiles` does not exist. Use `PUT /api/profiles/{newname}`.

### 4.3 No single-profile GET

Clients need to fetch the array and filter locally.

---

## 5. Shape mismatch vs. go2rtc

The carvilon-server's previous admin path called go2rtc's
`GET /api/streams`. The two response shapes are fundamentally
incompatible:

### 5.1 go2rtc `GET /api/streams`

Returns a **JSON object** (map) keyed by stream name. Each value has
exactly two fields, both produced by go2rtc's `Stream.MarshalJSON`:

```go
// go2rtc/internal/streams/stream.go
func (s *Stream) MarshalJSON() ([]byte, error) {
    var info = struct {
        Producers []*Producer   `json:"producers"`
        Consumers []core.Consumer `json:"consumers"`
    }{...}
    return json.Marshal(info)
}
```

Wire example (shape only):

```json
{
  "intercom": {
    "producers": [{ "url": "rtsp://...", ... }],
    "consumers": []
  },
  "ai360": {
    "producers": [...],
    "consumers": [...]
  }
}
```

### 5.2 Our `GET /api/profiles`

Returns a **JSON array** of profile-configuration objects. Each entry has
profile metadata (name, camera, quality, usage, codec, encode params) and
does NOT have producers / consumers — those concepts live separately in
`GET /stream/stats` (per-profile + per-client throughput counters).

### 5.3 What the carvilon-admin sees today

If the carvilon-admin client does:

```go
var streams map[string]StreamInfo  // go2rtc-shape
if err := json.Unmarshal(body, &streams); err != nil { ... }
```

and `body` is our `[{...},{...},...]` array, the unmarshal fails with
"cannot unmarshal array into Go value of type map[string]...". That's
the most likely path to the live "streams: profile not found" + "Aktive
Profile (0)" the briefing reports — the admin client treats the parse
failure as "no streams" and renders an empty list.

A second possibility: the admin client is still calling the literal path
`/api/streams` (which we never registered). Our mux falls through that
path to the embedded `web/` file server — which returns a 404 HTML page
or the spike's `index.html`. Either way the admin client sees garbage
and reports "0 profiles".

**This is for the master-chat to decide:** either the carvilon-side
client switches to call our `GET /api/profiles` and parse the array (the
S5/S6 contract), or we add a go2rtc-shape-compatible adapter endpoint
on our side. Both are achievable; the briefing's scope (§6) explicitly
excludes either fix from this S6-08 inventory.

---

## 6. Quick reference table

| Method | Path                        | Body          | Response                | Notes |
| ------ | --------------------------- | ------------- | ----------------------- | ----- |
| GET    | `/api/profiles`             | —             | JSON array (camelCase)  | profile list |
| PUT    | `/api/profiles/{name}`      | JSON snake_case | 204 / 400 / 503     | upsert |
| DELETE | `/api/profiles/{name}`      | —             | 204 / 404 / 503         | live viewers keep going |
| POST   | `/offer?src=<name>`         | SDP offer     | `application/sdp` answer | h264_passthrough only |
| GET    | `/api/stream.mjpeg?src=<name>` | —          | `multipart/x-mixed-replace` | mjpeg only |
| GET    | `/stream/h264?src=<name>`   | —             | `video/h264` chunked    | h264_cbp only |
| GET    | `/stream/stats`             | —             | JSON snapshot           | telemetry |
| GET    | `/healthz`                  | —             | 204                     | liveness |
