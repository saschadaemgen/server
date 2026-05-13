-- Saison 13-02-FIX4-c: ESP-Discovery + Adoption.
--
-- Neue Geraete melden sich via POST /esp/discover an; der Server
-- legt sie in esp_pending_devices ab und der Admin entscheidet
-- ueber Adoption oder Reject im /a/esp-viewers-Tab.
--
-- Spalten ueber dem Briefing-Vorschlag (Briefing F):
--   rejected_at              - NULL solange noch nicht rejected.
--                              Wird gesetzt wenn der Admin auf
--                              Reject klickt; bleibt in der
--                              Tabelle, damit der naechste ESP-
--                              Status-Poll noch "rejected"
--                              liefern kann.
--   adopted_token_cleartext  - Klartext-Bearer-Token, der nach
--                              Adopt zwischenparkt wird, bis der
--                              ESP ihn beim naechsten Status-
--                              Poll abholt. Danach wird die
--                              ganze Zeile geloescht (viewers
--                              haelt nur den Hash).

CREATE TABLE esp_pending_devices (
    mac                     TEXT PRIMARY KEY NOT NULL,
    model                   TEXT,
    fw_version              TEXT,
    capabilities            TEXT,
    discovered_at           INTEGER NOT NULL,
    last_poll_at            INTEGER NOT NULL,
    rejected_at             INTEGER,
    adopted_token_cleartext TEXT
);

CREATE INDEX idx_esp_pending_discovered ON esp_pending_devices(discovered_at);
CREATE INDEX idx_esp_pending_last_poll  ON esp_pending_devices(last_poll_at);

INSERT INTO schema_version (version, applied_at)
VALUES (9, strftime('%s','now')*1000);
