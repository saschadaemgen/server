-- Shelly Etappe 2 (mDNS Auto-Discovery + klebriges Entfernen): die
-- konfigurierte Menge der Shelly-Geraete wird von einer einzelnen
-- kommaseparierten Adressliste (platform_config key
-- shelly_device_addresses, Etappe 1) auf eine echte Tabelle gehoben.
-- Diese Tabelle ist ab jetzt die EINZIGE Wahrheit fuer die konfigurierte
-- Menge UND fuer die Ignorier-Liste - manuelle IPs und per mDNS gefundene
-- Geraete leben in derselben Menge, unterschieden nur durch die Herkunft.
--
--   mac      - die normalisierte Geraete-ID/MAC (Grossbuchstaben-Hex ohne
--              Trenner, z.B. "08F9E0E5C790"), '' solange unbekannt (ein
--              manuell getippter, nie erreichter IP-Eintrag). Sie ist die
--              stabile Identitaet fuer die klebrige Ignorier-Liste: ein
--              entferntes Geraet bleibt ueber MAC weg, auch wenn sich per
--              DHCP seine Adresse aendert.
--   address  - die LAN-IPv4[:port]-Adresse, unter der wir das Geraet per
--              lokalem RPC ansprechen (kanonisiert, LAN-gepruefte Form).
--   origin   - 'manual' (aus der Settings-IP-Liste) | 'discovered' (per
--              mDNS gefunden). Rein informativ + fuer die Manuell-Liste-
--              Rekonziliation; beide Herkuenfte werden gleich gepollt.
--   state    - 'active' (wird gepollt + angezeigt) | 'ignored' (klebrig
--              entfernt: NIE wieder automatisch uebernehmen, bis der Admin
--              es aus der Ignorier-Liste freigibt). Ein ignoriertes Geraet
--              bleibt als Zeile bestehen (nicht geloescht), damit Discovery
--              es an MAC ODER Adresse wiedererkennt und ueberspringt.
--   name /   - zuletzt gesehener Anzeigename/Modell (kosmetisch, aus
--   model      GetDeviceInfo bzw. dem mDNS-Announcement); '' erlaubt.
--
-- Eindeutigkeit: eine MAC darf hoechstens einmal vorkommen (partieller
-- UNIQUE-Index, leere MACs ausgenommen - mehrere noch-unbekannte manuelle
-- Eintraege duerfen koexistieren). Adressen-Dedupe passiert im Store-Code
-- (eine aktive Adresse ist eindeutig, aber eine ignorierte darf dieselbe
-- Adresse tragen wie ein spaeter neu getippter manueller Eintrag).
--
-- Die Legacy-Adressliste (shelly_device_addresses) wird NICHT in dieser
-- Migration geparst (SQL kann die Kommaliste nicht sauber zerlegen); das
-- uebernimmt ein einmaliger Seed in main (per Flag shelly_devices_migrated
-- gegen Wiederholung geschuetzt), damit ein spaeter geleerte Liste nicht
-- bei jedem Neustart wieder auferstehen kann.

CREATE TABLE shelly_devices (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    mac           TEXT NOT NULL DEFAULT '',
    address       TEXT NOT NULL,
    origin        TEXT NOT NULL,
    state         TEXT NOT NULL DEFAULT 'active',
    name          TEXT NOT NULL DEFAULT '',
    model         TEXT NOT NULL DEFAULT '',
    first_seen_at INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL
);

-- Eine MAC hoechstens einmal (leere ausgenommen): kein Duplikat fuer
-- dasselbe physische Geraet, egal ob per Adresse oder MAC hereingekommen.
CREATE UNIQUE INDEX shelly_devices_mac ON shelly_devices(mac) WHERE mac <> '';
CREATE INDEX shelly_devices_address ON shelly_devices(address);
CREATE INDEX shelly_devices_state ON shelly_devices(state);

INSERT INTO schema_version (version, applied_at)
VALUES (38, strftime('%s','now')*1000);
