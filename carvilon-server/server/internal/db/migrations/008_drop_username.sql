-- Saison 13-02-FIX4-a-HOTFIX4: viewers.username-Spalte abschaffen.
--
-- FIX4-b hatte fuer Web-Viewer einen separaten Username-Slot,
-- HOTFIX1/HOTFIX3 haben die Spalte hin und her normalisiert.
-- HOTFIX4 ist die radikale Vereinfachung: der Wohnungs-Name
-- IST der Login. Mieter tippt "Familie Mueller 2OG", Server
-- vergleicht case-insensitive + umlaut-tolerant + whitespace-
-- tolerant gegen viewers.name.
--
-- SQLite >= 3.35.0 unterstuetzt ALTER TABLE DROP COLUMN ohne
-- den klassischen "neue-Tabelle-und-kopieren"-Tanz. modernc.org/
-- sqlite (>= v1.50) bringt eine neuere Engine mit und kann das.
-- Vorteil: FK-Referenzen in viewer_sessions und door_events
-- bleiben unangetastet, kein FK-Disable-Trick noetig.
--
-- Der zugehoerige Index idx_viewers_username wird beim
-- Spalten-Drop automatisch mitgenommen.

DROP INDEX IF EXISTS idx_viewers_username;

ALTER TABLE viewers DROP COLUMN username;

INSERT INTO schema_version (version, applied_at)
VALUES (8, strftime('%s','now')*1000);
