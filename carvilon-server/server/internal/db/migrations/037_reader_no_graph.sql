-- Reader-Modell-Korrektur: ein Leser ist ein Palette-Baustein + ein
-- Registratur-Eintrag, KEIN Graph im Ordnerbaum. Diese Migration
--   (1) entfernt die graph_id-Spalte aus readers (der Reader-Graph-Seam
--       ist raus) - da graph_id einen Fremdschluessel auf designer_graphs
--       trug, geht das nur ueber einen Tabellen-Neubau (SQLite kann eine
--       Spalte mit FK nicht per DROP COLUMN entfernen);
--   (2) ergaenzt custom_name fuer die optionale Umbenennung im NFC-Menue
--       (leer => der sprechende Auto-Name gilt);
--   (3) raeumt bereits angelegte Reader-Graphen aus dem geschuetzten
--       System/Reader-Ordner (Migration 036 hatte je Leser einen Graphen
--       erzeugt; der Ordner bleibt, aber leer und geschuetzt).
--
-- Der Neubau laeuft mit foreign_keys=OFF (Migrations-Loop, s. migrate.go);
-- readers referenziert danach keine andere Tabelle mehr.

CREATE TABLE readers_new (
    id            TEXT PRIMARY KEY,
    kind          TEXT NOT NULL,
    model         TEXT NOT NULL DEFAULT '',
    firmware      TEXT NOT NULL DEFAULT '',
    bus           TEXT NOT NULL DEFAULT '',
    name          TEXT NOT NULL,
    custom_name   TEXT NOT NULL DEFAULT '',
    online        INTEGER NOT NULL DEFAULT 0,
    last_uid      TEXT NOT NULL DEFAULT '',
    last_seen_at  INTEGER,
    first_seen_at INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL
);

INSERT INTO readers_new
    (id, kind, model, firmware, bus, name, online, last_uid, last_seen_at, first_seen_at, updated_at)
SELECT
    id, kind, model, firmware, bus, name, online, last_uid, last_seen_at, first_seen_at, updated_at
FROM readers;

DROP TABLE readers;
ALTER TABLE readers_new RENAME TO readers;

-- Reader-Graphen aus dem System/Reader-Ordner entfernen (der Ordner
-- selbst bleibt als geschuetztes Geruest bestehen).
DELETE FROM designer_graphs
 WHERE folder_id IN (
     SELECT id FROM designer_folders WHERE name = 'Reader' AND system = 1
 );

INSERT INTO schema_version (version, applied_at)
VALUES (37, strftime('%s','now')*1000);
