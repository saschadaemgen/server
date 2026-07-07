-- Shelly Etappe 3 (Shelly im Logik-Editor), Phase 1: MQTT-Auto-
-- Provisionierung bei Freigabe. Ein freigegebenes Shelly bekommt ein
-- eigenes Broker-Geraetekonto und wird per HTTP so konfiguriert, dass es
-- sich am CARVILON-Broker anmeldet (TLS, user-CA) und Zustand/Messwerte
-- pusht. Diese Migration ergaenzt die drei Spalten, mit denen die
-- Geraeteseite den Provisionierungs-Stand je Geraet fuehrt und anzeigt.
--
--   mqtt_username   - der Broker-Geraete-Benutzername, den CARVILON diesem
--                     Shelly zugewiesen hat (z.B. "shelly-08f9e0e5c790").
--                     Zugleich das ACL-Subtree-Segment carvilon/<user>/#.
--                     '' solange nie provisioniert.
--   mqtt_state      - '' (nie) | 'provisioning' (laeuft) | 'linked' (am
--                     Broker konfiguriert) | 'failed' (Provisionierung
--                     fehlgeschlagen). Rein CARVILON-seitiger Merker; ob
--                     das Geraet aktuell verbunden ist, sagt der Broker.
--   mqtt_updated_at - Zeitpunkt der letzten Zustandsaenderung (ms).
--
-- Das Broker-Passwort selbst wird NICHT hier gespeichert: der Broker haelt
-- nur den Argon2id-Hash, das Geraet das Klartext-Passwort (einmalig
-- gepusht) - CARVILON braucht es nach dem Push nicht mehr aufzubewahren.

ALTER TABLE shelly_devices ADD COLUMN mqtt_username TEXT NOT NULL DEFAULT '';
ALTER TABLE shelly_devices ADD COLUMN mqtt_state TEXT NOT NULL DEFAULT '';
ALTER TABLE shelly_devices ADD COLUMN mqtt_updated_at INTEGER NOT NULL DEFAULT 0;

INSERT INTO schema_version (version, applied_at)
VALUES (39, strftime('%s','now')*1000);
