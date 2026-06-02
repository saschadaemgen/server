# Stream-Server Security (CARVILON video media layer)

**Status:** Created end of Stream season 3. The LOCAL security view of the
media layer. The shared token/crypto wire format is owned centrally - see
**secure-chat-wire-format.md**; this doc does NOT duplicate it, it records what
THIS layer enforces and where it relies on the central spec.

> Language policy: source/docs English, chat workflow German.
> NO secret values anywhere in this doc - keys are named abstractly only
> (e.g. "32-byte hex from CARVILON_..._HMAC_KEY").

---

## 1. Security mandate of this layer

Two things: (a) **encryption at the source/connection level** - the
camera->server hop (RTSPS, optionally SRTP/SDES) and the cloud TLS hops
(WHIP/WHEP TLS, turns: TLS); and (b) the **access tokens at both ends of the
cloud path** - a publish token at the WHIP ingress, an egress token at the
WHEP egress. This layer is a DUMB media layer: it verifies tokens and
terminates TLS, but it owns no tenants/users - the Master (carvilon-server)
mints all tokens and owns all authorization.

## 2. Threats addressed - and deliberately not addressed (honest)

**Addressed:**
- Unauthorized publish -> publish_token verify at the WHIP ingress (bare 401).
- Unauthorized pull -> egress_token verify at the WHEP egress (bare 401).
- Push/pull confusion -> SEPARATE HMAC keys; a publish token cannot pull and
  vice versa (proven: publish-key token -> signature mismatch at the egress).
- Unauthorized cold-trigger -> the egress 401 runs BEFORE the request_publish
  trigger, so a stranger cannot force the edge to publish.
- TURN relay abuse -> short-lived REST credentials (HMAC, TTL ~5 min); the
  long-term shared secret stays cloud-side.
- turns: cert trust -> a publicly-trusted cert on a hostname (a private-CA
  cert on a bare IP is rejected by the client's system pool).

**NOT addressed (today):**
- Asymmetric signatures: tokens are symmetric HMAC-SHA256. A holder of the
  HMAC key can mint tokens. Swap to JWS/asymmetric is open debt (jointly with
  publish_token).
- Per-client binding: the egress token is bound to a streamID + expiry, NOT to
  a single client - a leaked token is valid for ~5 min for that sid.
- LAN trust: inside the LAN the edge/server are mutually trusted; no intra-LAN
  authz beyond the tokens.
- Egress rate-limiting: not implemented (a valid token is not throttled).

## 3. Repo-specific hardening

- **publish_token** verify at `POST /whip/{streamID}` (internal/whip), and
  **egress_token** verify at `POST /whep/{streamID}` - both reuse the existing
  `publishtoken.Verify`, each with its own key, bare 401, concrete failure
  class logged server-side ONLY (no verification oracle), egress FAILS CLOSED
  when no key is configured.
- **TURN credentials**: RFC-TURN-REST ephemeral (HMAC over the shared secret,
  TTL ~5 min), minted per accepted peer; never reused, never stored beyond the
  minter closure.
- **TLS separation**: the WHIP/WHEP ingress uses the private **cloudca** cert
  (internal CA); the public **turns:** leg uses a separate publicly-trusted
  cert (e.g. Let's Encrypt for the relay hostname). The two never share a cert.
- **No secrets in logs**: keys/tokens are never logged or echoed - the code
  logs only the env NAME and the byte length on a config error; pion's TURN
  logging is discarded; the masked-IP ICE debug never logs a full address.

## 4. Interfaces to other layers

- The **Master (carvilon-server)** mints both tokens (publish + egress) and
  decodes the HMAC keys, passing them into this layer via `CloudSetupOptions`
  (`HMACKey`, `EgressHMACKey`) plus the TURN config. This layer never reads
  those env vars itself in the embedded build and never imports a tenant type.
- For every SHARED format (token layout, HMAC scheme, future asymmetric swap)
  -> **secure-chat-wire-format.md** is the single source of truth.
- The edge whipclient receives its TURN ICEServers (incl. short-lived creds)
  in the side-channel `request_publish` frame; the future Android subscriber
  must receive its ICEServers the same way (Master-side), see the feature
  backlog.

## 5. Open security debt + season target

- **JWS-to-asymmetric swap** for both tokens (symmetric HMAC today). Target:
  jointly with the publish_token swap; coordinate with the Master / the
  central spec (target season TBD - name it when the Master sets it).
- **Egress rate-limiting** and **per-client token binding** (the egress token
  is sid-bound, not client-bound, ~5 min).
- **Key hygiene**: during the season-3 test the HMAC keys were briefly handled
  outside the server env on the test network (minting test tokens on the
  desktop). They are test-net keys, but as good hygiene **rotate the HMAC keys
  (publish + egress) and the TURN shared secret before production use.**

## 6. Season references

Season 3 created this doc. Introduced in season 3: the WHEP **egress-auth**
(separate key, fail-closed, 401 before the cold-trigger - D-0008), the **TLS
separation** (cloudca internal vs Let's-Encrypt turns: external - D-0005), and
the **TURN ephemeral credentials** (HMAC-REST, TTL ~5 min - D-0005). Earlier:
source SRTP/SDES (season 1, D-0001) and the publish_token at the WHIP ingress
(season 2).

---

*Living document. Last: end of Stream season 3. Local view; shared crypto in
secure-chat-wire-format.md. Siblings: stream-server-decisions.md,
stream-server-architecture.md.*
