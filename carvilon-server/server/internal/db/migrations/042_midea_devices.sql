-- Migration 042 (Saison 21 - Midea Climate Controller, Etappe 1): the
-- persistent set of Midea/Springer split-AC units the CARVILON server has
-- discovered, adopted or ignored, plus their ENCRYPTED V3 credentials so an
-- adopted device survives a server restart and is re-provisioned from here.
-- Mirrors shelly_devices (038): one durable identity per device, an approval
-- gate (pending -> active | ignored), and a sticky ignore list.
--
--   id           - the stable device identity: the lowercase hex of the native
--                  Midea 6-byte device id. Address-independent (survives a DHCP
--                  change), so the ignore list and the adopted set both key on
--                  it. Discovery finds this locally; the credentials do not.
--   device_id    - the native Midea device id as an integer (what the LAN
--                  8370-handshake and the cloud token lookup use).
--   address      - last-seen LAN IPv4 the device answered discovery from; the
--                  address the adapter dials for status/control.
--   name         - last-seen discovery name (e.g. "net_ac_xxxx"), display only.
--   protocol_v3  - 1 = V3 (token/key required, cloud-provisioned) | 0 = V2.
--   origin       - 'discovered' (local UDP discovery) | 'manual' (typed IP).
--   state        - 'pending' (found, never contacted, waits for approval) |
--                  'active' (adopted, credentials present, polled + controllable) |
--                  'ignored' (sticky-removed / rejected, never auto-adopted).
--   profile      - 'standard' (device-side control, remote-like; E1 default) |
--                  'advanced' (server-side control loop; unlocked in a later
--                  etappe). Per-device toggle, standard is the safe default.
--   token_enc /  - the V3 token and key, AES-256-GCM encrypted (internal/secrets)
--   key_enc        over the hex form. NEVER stored in plaintext; empty until the
--                  device is adopted. This is the only credential material on
--                  disk and it is provisioned, never committed.
--   first_seen_at/updated_at - ms epoch bookkeeping.
CREATE TABLE midea_devices (
    id            TEXT PRIMARY KEY,
    device_id     INTEGER NOT NULL,
    address       TEXT NOT NULL DEFAULT '',
    name          TEXT NOT NULL DEFAULT '',
    protocol_v3   INTEGER NOT NULL DEFAULT 1,
    origin        TEXT NOT NULL DEFAULT 'discovered',
    state         TEXT NOT NULL DEFAULT 'pending',
    profile       TEXT NOT NULL DEFAULT 'standard',
    token_enc     TEXT NOT NULL DEFAULT '',
    key_enc       TEXT NOT NULL DEFAULT '',
    first_seen_at INTEGER NOT NULL DEFAULT 0,
    updated_at    INTEGER NOT NULL DEFAULT 0
);

INSERT INTO schema_version (version, applied_at)
VALUES (42, strftime('%s','now')*1000);
