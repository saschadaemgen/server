-- Benutzerverwaltung-Korrektur: ein UA-Profil darf hoechstens an EINEN
-- CARVILON-Benutzer geknuepft sein.
--
-- Das richtige Modell: CARVILON ist die Master-Datenbank, es gibt genau
-- eine Benutzerliste (die CARVILON-Benutzer). Ein UA-User ist kein
-- eigener Eintrag, sondern nur eine optionale Verknuepfung (ua_user_id)
-- an einem CARVILON-Benutzer - die Bruecke, ueber die ein UA-Reader-
-- Ereignis ("Person Z an Reader Y") zurueck auf unseren Benutzer
-- uebersetzt wird.
--
-- Damit diese Uebersetzung eindeutig bleibt, darf dieselbe UA-Identitaet
-- nicht an zwei CARVILON-Benutzer haengen. Ein partieller UNIQUE-Index
-- (nur wo ua_user_id gesetzt ist) erzwingt das auf DB-Ebene - auch gegen
-- Races, die eine reine Handler-Pruefung nicht abfaengt. NULL bleibt
-- beliebig oft erlaubt (nicht verknuepfte Benutzer).

CREATE UNIQUE INDEX idx_carvilon_users_ua_link
    ON carvilon_users (ua_user_id)
    WHERE ua_user_id IS NOT NULL;

INSERT INTO schema_version (version, applied_at)
VALUES (35, strftime('%s','now')*1000);
