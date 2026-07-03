-- Designer-Persistenz: Ordnerbaum + Graphen des Logik-Editors.
--
-- Der Ordnerbaum oben links im Editor (bisher reine Demo in palette.js)
-- wird echt: Ordner und Graph-Objekte liegen in SQLite, der Editor
-- speichert per Autosave. Zugleich das Fundament fuer Komponenten:
-- geschuetzte System-Ordner (system=1), deren Inhalte spaeter in
-- Hauptmenue-Seiten gespiegelt werden (Tags/RFID-Track) und per
-- Deep-Link (?g=<id>) in den Editor fuehren. System-Ordner sind nicht
-- loeschbar, nicht umbenennbar, nicht verschiebbar; manuelles Anlegen
-- von Graphen darin ist gesperrt (Reader entstehen spaeter ueber ihren
-- eigenen Flow). Die Regeln erzwingt internal/designerstore.
--
--   designer_folders - ein Knoten des Baums. parent_id NULL = Wurzel.
--       sort ordnet Geschwister (dann Name); system=1 markiert
--       geschuetzte Ordner. ON DELETE RESTRICT haelt die Struktur
--       konsistent, auch wenn der Store "nur leere Ordner loeschen"
--       bereits selbst prueft.
--
--   designer_graphs - ein gespeicherter Graph. graph_json ist das
--       Editor-Format (schema/nodes/edges mit Positionen, Farben,
--       Props) - fuer den Server opak; die Engine bekommt weiterhin
--       nur das kanonische Run-Format vom Editor gePOSTet. rev zaehlt
--       serverseitig bei jedem Speichern hoch (last-write-wins, ein
--       Admin); der Editor zeigt es in der Statusleiste.
--
-- Seed: die bisherige Demo-Struktur als echte, leere Struktur (Namen
-- wie im Demo) plus der geschuetzte Ordner System > Reader.

CREATE TABLE designer_folders (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    parent_id  INTEGER REFERENCES designer_folders(id) ON DELETE RESTRICT,
    name       TEXT NOT NULL,
    system     INTEGER NOT NULL DEFAULT 0,
    sort       INTEGER NOT NULL DEFAULT 0,
    updated_at INTEGER NOT NULL
);

CREATE INDEX idx_designer_folders_parent ON designer_folders(parent_id);

CREATE TABLE designer_graphs (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    folder_id  INTEGER NOT NULL REFERENCES designer_folders(id) ON DELETE RESTRICT,
    name       TEXT NOT NULL,
    graph_json TEXT NOT NULL DEFAULT '{"schema":1,"nodes":[],"edges":[]}',
    rev        INTEGER NOT NULL DEFAULT 0,
    sort       INTEGER NOT NULL DEFAULT 0,
    updated_at INTEGER NOT NULL
);

CREATE INDEX idx_designer_graphs_folder ON designer_graphs(folder_id);

INSERT INTO designer_folders (id, parent_id, name, system, sort, updated_at) VALUES
    (1, NULL, 'Building',     0, 0, strftime('%s','now')*1000),
    (2, 1,    'Ground floor', 0, 0, strftime('%s','now')*1000),
    (3, 1,    'First floor',  0, 1, strftime('%s','now')*1000),
    (4, NULL, 'Sandbox',      0, 1, strftime('%s','now')*1000),
    (5, NULL, 'System',       1, 2, strftime('%s','now')*1000),
    (6, 5,    'Reader',       1, 0, strftime('%s','now')*1000);

INSERT INTO designer_graphs (folder_id, name, sort, updated_at) VALUES
    (2, 'EG · Flur',    0, strftime('%s','now')*1000),
    (2, 'EG · Living',  1, strftime('%s','now')*1000),
    (2, 'EG · Kitchen', 2, strftime('%s','now')*1000),
    (3, 'OG · Hall',    0, strftime('%s','now')*1000),
    (3, 'OG · Bath',    1, strftime('%s','now')*1000),
    (4, 'Test rig',     0, strftime('%s','now')*1000);

INSERT INTO schema_version (version, applied_at)
VALUES (32, strftime('%s','now')*1000);
