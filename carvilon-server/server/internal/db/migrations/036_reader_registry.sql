-- Reader-Registratur (NFC-Track, Geraete-Ebene): jeder beim Start
-- erkannte Tag-Leser wird als geschuetzte Komponente hier registriert.
-- Die Tabelle ist die Quelle der Wahrheit fuer die NFC-Menue-Seite und
-- speist zugleich einen strukturgesperrten Graphen im System/Reader-
-- Ordner (graph_id), sodass der Sprung in den Logik-Editor ein Ziel hat.
--
--   id            - stabile Identitaet, abgeleitet aus Modalitaet + Bus
--                   (z.B. "nfc:i2c-1"). Der PN532 hat keine eindeutige
--                   Chip-Seriennummer; der Bus ist die stabile Kennung,
--                   model/firmware sind die begleitende Chip-Info. Nach
--                   Neustart derselbe Bus => dieselbe id => kein Duplikat.
--   kind          - Reader-Modalitaet ("nfc"). Der Diskriminator, der die
--                   Registratur fuer weitere Reader-Typen (UA-Reader,
--                   eigenes Ticket) offen haelt - NICHT PN532-fest.
--   online        - 1 solange die Hardware beim letzten Start erkannt
--                   wurde; verschwindet der Leser, bleibt der Eintrag auf
--                   0 stehen (nie geloescht) - man soll ein fehlendes
--                   Geraet sehen. Taucht er wieder auf, zurueck auf 1.
--   graph_id      - der zugehoerige System/Reader-Graph (ON DELETE SET
--                   NULL; der Graph liegt im gesperrten Ordner und kann
--                   ohnehin nicht per UI geloescht werden).
--   last_uid /    - zuletzt gesehenes Tag plus Zeitstempel (ms), vom
--   last_seen_at    laufenden nfc-Treiber ueber den Tag-Observer gefuellt;
--                   NULL solange nie ein Tag gelesen wurde.
--   first_seen_at - erste Registrierung (ms), bleibt ueber Neustarts hin
--                   erhalten.

CREATE TABLE readers (
    id            TEXT PRIMARY KEY,
    kind          TEXT NOT NULL,
    model         TEXT NOT NULL DEFAULT '',
    firmware      TEXT NOT NULL DEFAULT '',
    bus           TEXT NOT NULL DEFAULT '',
    name          TEXT NOT NULL,
    online        INTEGER NOT NULL DEFAULT 0,
    graph_id      INTEGER REFERENCES designer_graphs(id) ON DELETE SET NULL,
    last_uid      TEXT NOT NULL DEFAULT '',
    last_seen_at  INTEGER,
    first_seen_at INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL
);

INSERT INTO schema_version (version, applied_at)
VALUES (36, strftime('%s','now')*1000);
