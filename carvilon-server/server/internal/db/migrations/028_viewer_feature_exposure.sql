-- Saison 20: Variante A - eine dreistufige Sichtbarkeit pro (Viewer, Funktion).
--
-- viewer_feature_exposure ersetzt BEIDE getrennten Achsen:
--   - viewer_setting_visibility (022, live): visible_to_tenant 0/1
--   - viewer_feature_active     (027, ungepusht): active 0/1
--
-- exposure ist eine FREIE TEXT-Spalte (KEIN CHECK), in Go validiert. Heute
-- gueltig: 'tenant_visible' | 'admin_only' | 'hidden'. Grund: der spaetere
-- zweite "ausblenden"-Wert 'bookable' (kostenpflichtig, noch nicht gebucht)
-- kostet so NULL Migration - nur ein Eintrag in die Go-Wertemenge. 'bookable'
-- ist reservierter Platz OHNE Logik (Abo-/Lizenz-Server spaeter).
--
-- Mieter-Sicht: tenant_visible -> sichtbar+editierbar+write-back; admin_only und
-- hidden -> unsichtbar. Wert: hidden -> Katalog-Standard (Override ignoriert);
-- admin_only -> Viewer ?? Vorlage ?? Standard; tenant_visible -> wie bisher.
-- Default ohne Zeile = tenant_visible (= heutiges Verhalten, kein Bruch).

CREATE TABLE viewer_feature_exposure (
    viewer_mac  TEXT NOT NULL REFERENCES viewers(mac) ON DELETE CASCADE,
    feature_key TEXT NOT NULL,                -- spannt Legacy- + Katalog-Keys
    exposure    TEXT NOT NULL,                -- frei, Go-validiert (kein CHECK)
    PRIMARY KEY (viewer_mac, feature_key)
);
CREATE INDEX idx_viewer_feature_exposure_mac ON viewer_feature_exposure(viewer_mac);

-- 022-Daten migrieren: 0 -> admin_only, 1 -> tenant_visible. Alle Zeilen
-- explizit, damit der abgeleitete visibility-Block byte-gleich bleibt.
INSERT INTO viewer_feature_exposure (viewer_mac, feature_key, exposure)
SELECT viewer_mac, setting_key,
       CASE WHEN visible_to_tenant = 0 THEN 'admin_only' ELSE 'tenant_visible' END
FROM viewer_setting_visibility;

-- Alte Strukturen abloesen (Owner-Erlaubnis). viewer_feature_active (027) ist
-- ungepusht -> keine veroeffentlichten Daten; Default tenant_visible stellt das
-- heutige keep_stream-Verhalten wieder her.
DROP TABLE viewer_setting_visibility;
DROP TABLE viewer_feature_active;

-- template_features: active(INTEGER) -> exposure(TEXT, NULL=erbt). value bleibt.
-- 027 ungepusht -> kein Daten-Mapping noetig.
ALTER TABLE template_features ADD COLUMN exposure TEXT;
ALTER TABLE template_features DROP COLUMN active;

INSERT INTO schema_version (version, applied_at)
VALUES (28, strftime('%s','now')*1000);
