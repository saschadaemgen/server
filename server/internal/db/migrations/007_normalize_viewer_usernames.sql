-- Saison 13-02-FIX4-a-HOTFIX3: symmetric username normalization.
--
-- HOTFIX1 normalisierte den Login-Input (Dämgen -> daemgen),
-- liess die viewers.username-Spalte aber unangetastet. SQLites
-- LOWER('Dämgen') ergibt 'dämgen' (nicht 'daemgen'), also
-- matched der Lookup nicht. Diese Migration normalisiert die
-- Bestands-Eintraege auf das gleiche Format wie sanitizeUsername
-- in Go: lowercase + germanic Umlaute zu ae/oe/ue/ss.
--
-- Sonderzeichen-Filter (a-z 0-9 _ . -) macht die Migration NICHT;
-- Bestands-Eintraege sollten sowieso einfache Namen sein, und
-- der admin-Anlegen-Pfad rennt seit HOTFIX1 schon durch
-- sanitizeUsername.
--
-- Idempotent: wenn der Username bereits normalisiert ist, ist
-- das UPDATE ein No-Op. Migration laeuft beim naechsten Start
-- nicht erneut (schema_version-Tracking).

UPDATE viewers
   SET username = LOWER(
       REPLACE(
         REPLACE(
           REPLACE(
             REPLACE(
               REPLACE(
                 REPLACE(
                   REPLACE(username,
                     'ä', 'ae'),
                   'ö', 'oe'),
                 'ü', 'ue'),
               'ß', 'ss'),
             'Ä', 'ae'),
           'Ö', 'oe'),
         'Ü', 'ue')
       )
 WHERE username IS NOT NULL;

INSERT INTO schema_version (version, applied_at)
VALUES (7, strftime('%s','now')*1000);
