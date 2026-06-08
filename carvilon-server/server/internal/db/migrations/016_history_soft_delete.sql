-- Saison 14-04-Phase2: Mieter-Soft-Delete + Capture-Toggle.
--
-- Zwei orthogonale Mechanismen fuer Mieter-Privatsphaere ohne
-- Admin-Audit zu kompromittieren:
--
-- 1. viewer_hidden_events  Soft-Delete pro Mieter: ein Eintrag
--                          taucht hier auf wenn der Mieter ihn
--                          aus seinem Verlauf entfernt hat. Admin-
--                          Endpoints lesen door_events ungefiltert
--                          (Audit-Trail bleibt) und joinen gegen
--                          diese Tabelle nur um das "vom Mieter
--                          ausgeblendet"-Badge zu setzen.
--
--                          FK CASCADE auf viewers.mac stellt sicher
--                          dass Loeschen eines Viewers seine
--                          versteckten-Markierungen mitnimmt;
--                          FK CASCADE auf door_events.id sorgt
--                          dafuer dass Admin-Hard-Delete eines
--                          Events die Hidden-Reihen automatisch
--                          mitloescht (keine baumelnden Refs).
--
-- 2. viewers.history_capture_enabled  Mieter-Datenschutz-Toggle:
--                                     wenn 0, blendet die Mieter-
--                                     API alle Eintraege voellig
--                                     aus (mit capture_enabled-Flag
--                                     im JSON). Default 1 = aktiv
--                                     bei jedem neuen Viewer.
--                                     Admin sieht die Eintraege
--                                     trotzdem (Capture-Toggle ist
--                                     UI-only, die door_events-
--                                     Inserts laufen weiter via
--                                     doorbellhub.persistStart).
--
-- Resolver-Default im mockmanager ist true; gibt es NULL in der
-- DB, gilt "Erfassung aktiv". Der DDL-Default erspart einen
-- Code-Pfad fuer Pre-Migration-Eintraege.

CREATE TABLE IF NOT EXISTS viewer_hidden_events (
    viewer_mac TEXT    NOT NULL,
    event_id   INTEGER NOT NULL,
    hidden_at  INTEGER NOT NULL,
    PRIMARY KEY (viewer_mac, event_id),
    FOREIGN KEY (viewer_mac) REFERENCES viewers(mac)     ON DELETE CASCADE,
    FOREIGN KEY (event_id)   REFERENCES door_events(id)  ON DELETE CASCADE
);
CREATE INDEX idx_viewer_hidden_events_mac ON viewer_hidden_events(viewer_mac);

ALTER TABLE viewers ADD COLUMN history_capture_enabled INTEGER DEFAULT 1;

INSERT INTO schema_version (version, applied_at)
VALUES (16, strftime('%s','now')*1000);
