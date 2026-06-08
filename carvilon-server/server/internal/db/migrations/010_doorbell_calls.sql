-- Saison 13-03: aktiver Klingel-Anruf-Lifecycle.
--
-- door_events (Migration 005) ist der Audit-Trail mit langer
-- Lebensdauer; doorbell_calls ist der kurzlebige State pro
-- aktivem Anruf, an dem die Multi-Viewer-Logik
-- "answered_elsewhere" haengt. Der primary key event_id ist
-- der 32-stellige cancel_token aus dem MQTT-/remote_view-
-- Frame; jedes Web-Viewer- und ESP-SSE bekommt diesen Wert
-- mit jedem doorbell.ring-Event und schickt ihn unveraendert
-- zurueck wenn der Mieter Annehmen oder Ablehnen klickt.
--
-- answered_by und ended_by speichern die viewer_mac, die den
-- Anruf fuer sich beansprucht hat. Compare-and-set in der
-- MarkAnswered-Service-Methode garantiert dass genau ein
-- Viewer als erster gewinnt; die anderen sehen die row
-- bereits gesetzt und bekommen ein cancel mit reason=
-- answered_elsewhere.
--
-- cancel_reason wird beim Lifecycle-Ende gesetzt:
--   "timeout"            Klingel-Timeout (UDM /cancel_doorbell)
--   "rejected"           Mieter hat Ignorieren geklickt
--   "answered_elsewhere" anderer Viewer hat angenommen
--   "user_ended"         Mieter hat den Anruf beendet
--
-- Keine FK auf viewers.mac: ein laufender Anruf darf einen
-- viewer ueberleben (im Edge-Fall wird der Viewer waehrend
-- des Ringens geloescht).

CREATE TABLE doorbell_calls (
    event_id      TEXT PRIMARY KEY NOT NULL,
    viewer_mac    TEXT NOT NULL,
    device_id     TEXT,
    started_at    INTEGER NOT NULL,
    answered_by   TEXT,
    answered_at   INTEGER,
    ended_by      TEXT,
    ended_at      INTEGER,
    cancel_reason TEXT
);

CREATE INDEX idx_doorbell_calls_viewer  ON doorbell_calls(viewer_mac);
CREATE INDEX idx_doorbell_calls_started ON doorbell_calls(started_at);

INSERT INTO schema_version (version, applied_at)
VALUES (10, strftime('%s','now')*1000);
