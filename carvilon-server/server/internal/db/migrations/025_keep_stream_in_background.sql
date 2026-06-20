-- Saison 20: ESP "Stream im Hintergrund halten" - zwei
-- unabhaengige Toggles.
--
--   keep_stream_in_screensaver  INTEGER  Default 0 (false)
--   keep_stream_in_screen_off   INTEGER  Default 0 (false)
--
-- Zwei boolsche ESP-Hardware-Flags. Der ESP entscheidet selbst,
-- ob er den laufenden Stream beim Wechsel in den Bildschirmschoner
-- bzw. bei Display-aus offen haelt; carvilon speichert + liefert
-- die Flags nur. Web-Viewer ignorieren sie.
--
-- Anders als die uebrigen ESP-Settings (NULL + Resolver-Default)
-- bekommen diese zwei einen harten DDL-Default 0: ein frischer
-- ESP soll ohne gesetzten Wert das saubere Schliessen sehen
-- (= false). Der Resolver in viewermanager liefert zusaetzlich
-- false bei NULL, damit pre-Migration-Reihen das gleiche
-- Default-Verhalten zeigen.

ALTER TABLE viewers ADD COLUMN keep_stream_in_screensaver INTEGER DEFAULT 0;
ALTER TABLE viewers ADD COLUMN keep_stream_in_screen_off  INTEGER DEFAULT 0;

INSERT INTO schema_version (version, applied_at)
VALUES (25, strftime('%s','now')*1000);
