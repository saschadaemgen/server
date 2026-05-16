-- Saison 14-03: per-viewer auto-screensaver-Timer.
--
-- viewers.auto_screensaver_seconds steuert wie viele Sekunden
-- Inaktivitaet die Mieter-UI braucht bis sie automatisch zurueck
-- in den Bildschirmschoner-Modus slidet. NULL = Funktion
-- ausgeschaltet (Default). Sinnvolle Werte: 30, 60, 300, 600 -
-- die fuenf Optionen in der Inline-Settings-Form. Werte
-- ausserhalb dieser Menge werden vom POST-Handler abgelehnt.
--
-- Wenn der Mieter idle_view_mode='livestream' gesetzt hat,
-- ignoriert die Browser-Runtime den Timer auch bei gesetztem
-- Wert (Auto-Bildschirmschoner ergibt nur in Kombination mit
-- 'screensaver' als Idle-Default Sinn).

ALTER TABLE viewers ADD COLUMN auto_screensaver_seconds INTEGER;

INSERT INTO schema_version (version, applied_at)
VALUES (14, strftime('%s','now')*1000);
