-- Benutzerverwaltung-Fundament: CARVILONs eigene Benutzer.
--
-- Bewusste Umkehr der Saison-12-Maxime "Keine eigene Mieter-DB,
-- UA-User sind die Mieter" (siehe decisions-Eintrag): CARVILON ist
-- ein eigenstaendiges System und fuehrt ab hier seine eigenen
-- Benutzer als vollwertige Objekte. UA ist nur noch Hardware, wenn
-- wir sie nutzen; der CARVILON-Benutzer ist die Wahrheit.
--
--   carvilon_users - die Kern-Identitaetstabelle. Bewusst schlank
--       gehalten (Identitaet + aktiv + optionale UA-Kopplung):
--
--         id           stabile interne UUID (v4), die kanonische ID
--                      auf die spaetere Adapter (Lohn/Gehalt via
--                      plugin_data) verschluesselt referenzieren.
--         display_name der Anzeigename.
--         active       der Ein-/Ausschalter: wird dieser Benutzer
--                      ueberhaupt verwendet (1) oder ist er stillgelegt
--                      (0). Loeschen bleibt getrennt davon moeglich.
--         ua_user_id   OPTIONALE Kopplung an einen UA-User, WENN UA
--                      genutzt wird - sonst NULL. Die UA-Kopplung
--                      haengt am CARVILON-Benutzer, nicht umgekehrt.
--                      Keine FK: der UA-User lebt nicht in unserer DB,
--                      die ID ist ein opaker Fremd-Schluessel.
--         created_at / updated_at  Unix-Millis (wie die anderen Tabellen).
--
--   Lohn-/domaenenspezifische Felder (Personalnummer, Stundensatz, ...)
--   gehoeren NICHT hierher, sondern spaeter verschluesselt in plugin_data
--   auf die carvilon_users.id. So bringt ein spaeterer Adapter seine
--   eigenen Felder mit, ohne dieses Kern-Modell aufzubohren.

CREATE TABLE carvilon_users (
    id           TEXT PRIMARY KEY,
    display_name TEXT NOT NULL DEFAULT '',
    active       INTEGER NOT NULL DEFAULT 1,
    ua_user_id   TEXT,
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL
);

-- Schnelle Suche der aktiven Benutzer (Standard-Ansicht der
-- Benutzer-Seite ist "aktive zuerst").
CREATE INDEX idx_carvilon_users_active ON carvilon_users (active);

INSERT INTO schema_version (version, applied_at)
VALUES (34, strftime('%s','now')*1000);
