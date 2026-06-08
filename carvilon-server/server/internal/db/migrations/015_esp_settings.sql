-- Saison 14-XX: ESP-Settings persistierbar pro Viewer.
--
-- Drei neue Spalten in viewers, alle nullable. Resolver im
-- mockmanager liefern Defaults wenn NULL:
--
--   brightness_idle       INTEGER NULL  Default 70    (Range 0..100)
--   screen_off_after_sec  INTEGER NULL  Default 0     (0 = nie,
--                                                       sonst aus
--                                                       Allow-Liste
--                                                       {30,60,300,
--                                                        600,1800})
--   language              TEXT NULL     Default "de"  (Allow-Liste
--                                                       {"de","en"})
--
-- Keine harten DDL-Defaults: Resolver-Defaults im Code sind
-- sauberer und erlauben spaeter eine UI-Anzeige fuer "noch nicht
-- gesetzt" vs "explizit auf Default-Wert gesetzt".
--
-- brightness_idle und screen_off_after_sec sind ESP-Hardware-
-- Konzepte (Display-Helligkeit + Backlight-Off-Timer). Web-Viewer
-- ignorieren die Werte; language ist fuer beide Geraete-Typen
-- relevant.
--
-- Plus: idle_view_mode (Migration 013) bekommt einen dritten
-- erlaubten Wert "screen_off". Storage-seitig nichts zu tun -
-- die Spalte ist plain TEXT NULL ohne CHECK-Constraint - aber
-- die Validierung im mockmanager.SetIdleViewMode wird im
-- begleitenden Code-Patch erweitert.

ALTER TABLE viewers ADD COLUMN brightness_idle      INTEGER;
ALTER TABLE viewers ADD COLUMN screen_off_after_sec INTEGER;
ALTER TABLE viewers ADD COLUMN language             TEXT;

INSERT INTO schema_version (version, applied_at)
VALUES (15, strftime('%s','now')*1000);
