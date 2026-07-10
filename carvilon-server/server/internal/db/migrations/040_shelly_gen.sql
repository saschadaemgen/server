-- Shelly Gen1 (S21 Gen1 integration, M1): the device set learns which API
-- generation each device speaks, so the fleet can dispatch to the right
-- adapter (Gen2+ JSON-RPC vs Gen1 REST) and provisioning can pick the
-- right broker tier (TLS 8883 vs the documented plaintext-1883 tier).
--
--   gen - 0 (unknown: never identified; a manual IP that was never
--         reached), 1 (Gen1 REST/plaintext-MQTT device), 2+ (Gen2+ RPC
--         device, the value the device reports). The generation is
--         learned from the mDNS announcement (service type + name shape)
--         or from an identify probe (GET /shelly: Gen2+ carries a "gen"
--         field, Gen1 has none) and is refreshed, never guessed.
--
-- Backfill: every row discovered via mDNS to date came through the
-- _shelly._tcp browser, which is Gen2+ by definition (the coordinator
-- rejected gen < 2 announcements), and a row whose provisioning LINKED
-- went through the Gen2-only RPC flow successfully - both are safely
-- Gen2. A 'failed'/'provisioning' state proves nothing: the old Gen2-only
-- provisioning failed on exactly the devices that are NOT Gen2 (a
-- manually pinned Gen1 box has no /rpc), so tagging those rows gen=2
-- would strand a real Gen1 device on the wrong transport forever. They
-- stay 0 (unknown) and are classified by the next identify probe instead
-- of being guessed.

ALTER TABLE shelly_devices ADD COLUMN gen INTEGER NOT NULL DEFAULT 0;

UPDATE shelly_devices SET gen = 2 WHERE origin = 'discovered' OR mqtt_state = 'linked';

INSERT INTO schema_version (version, applied_at)
VALUES (40, strftime('%s','now')*1000);
