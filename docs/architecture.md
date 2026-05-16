# unifix Architecture

**Status:** Saison 14 laufend, 16. Mai 2026. S14-01 (Stream-
Backend go2rtc), S14-01b (Idle-View-Modus mit Bildschirmschoner,
open-meteo-Wetter, Mieter-Settings), S14-01-FIX01 (Stream-Proxy
URL-Hardening) und S14-01-FIX02 (ESP-Unlock-Auto-Resolution)
abgeschlossen. Vorheriger Stand: Saison 13 abgeschlossen
14. Mai 2026 (S13-DOC).
Lebendes Dokument, wird pro Saison ergaenzt.
**Geltungsbereich:** Interne Architektur-Entscheidungen, strategische
Eckpunkte. KEIN Marketing-Material, KEIN Open-Source-Hinweis.

## 1. Drei-Schichten-Plattform

```
UniFi-Welt (UDM + Hub Door + echte Intercom) <- bleibt unveraendert
   |
   | MQTT/mTLS, WebSocket/JWT, HTTPS-Adoption
   v
unifix-Host (RPi pro Standort)
   - mock-Viewer-Goroutines (im unifix-server-Prozess,
     simulieren UA Intercom Viewer fuer das UDM)
   - unifix-server (HTTPS, Admin- und Mieter-UI, SSE-Hub,
     Auth, Persistenz, UA-API-Client)
   - go2rtc (Stream-Bridge fuer RTSP-zu-Klient, Saison 13+)
   |
   | HTTP/HTTPS, Magic-Link, SSE
   v
Endgeraete (Browser, Tablet, ESP32, Smart-TV, ...)
```

## 2. Module im Monorepo

```
server/         Haupt-Produkt, Pool-Manager + REST + UI + DB
mock/           UA Intercom Viewer-Simulator
license-server/ Cloud-Komponente, Lizenz-Validation + Updates
shared/         types, proto (Wire-Format), logging
```

## 3. Sprache, Build, Deployment

```
Sprache:          Go, komplett (kein Python in Saison 10+)
Entwicklung:      Windows 11 nativ, Go 1.26.1
Build:            Cross-Compile linux/arm64 via PowerShell-Skripte
Lauflage:         RPi 4 oder 5 pro Kunden-Standort
Production-Flags: -s -w -trimpath, CGO_ENABLED=0
Source-Code:      geschlossen, kein Push zu Remote-Hostern
```

## 4. Lauflage-Topologie

```
Pro Kunden-Anlage:
   1x RPi mit unifix-server-Binary
   1x oder mehr UDM-SE als UniFi-Controller
   N x Mock-Viewer-Goroutines im unifix-server-Prozess
   N x Mieter-Endgeraete (Browser, ESP, etc.)

Pro Sascha-Kunde (Multi-Anlage):
   Mehrere RPis, jeweils einer pro Anlage
   Optional zentraler Verwaltungs-Cockpit (spaeter)

Lizenz-Server (spaetere Saison, zeitlich offen):
   Eine Cloud-Instanz, validiert Lizenzschluessel ALLER Kunden,
   verteilt Updates und CA-Cert-Bundles. Der ursprueglich fuer
   Saison 14 geplante Ausbau ist seit S13-DOC-00 in eine spaetere
   Saison verschoben; das Skelett im Repo bleibt erhalten.
```

## 5. Lebenszyklus eines Mock-Viewers (Saison-12-Endstand)

Saison-10-Vision sah hier einen Pool-Manager mit Subprozessen vor.
Diese ist durch die Saison-12-Architektur-Entscheidung abgeloest:
Mock-Viewer laufen als Goroutines im unifix-server-Prozess. Siehe
Sektion 10 fuer die Begruendung.

Aktueller Lebenszyklus eines Mock-Viewers:

1. Admin im unifix-Admin-UI (`/a/mocks`): "Neuen Mock-Viewer anlegen"
   (Name vom Admin frei vergeben, MAC optional; bei leer wird eine
   Ubiquiti-OUI-MAC generiert)
2. `mockmanager.AddViewer` persistiert in `mock_viewers`-Tabelle und
   reserviert einen freien Service-Port (Default-Start 8100)
3. `mockmanager` spawnt `mock.Viewer`-Goroutine mit Stages 1 und 4
4. Mock-Viewer antwortet auf UDM-Multicast-Discovery (Stage 1)
5. Admin im UA-Interface-Designer: "Verwenden"-Klick auf den
   neu entdeckten Mock
6. UDM schickt Adoption-POST an `Mock:port` (Stage 4)
7. `mock.Viewer` baut WS (Stage 5) und MQTT (Stage 6) auf
8. UDM sieht Mock als online (gruener Punkt)
9. Admin im unifix-Admin-UI: bei dem Mock-Viewer auf "Login-Link"
   klicken; ein Modal zeigt eine 24h-gueltige Magic-Link-URL
   (`/m/login?t=...`)
10. Admin verschickt den Magic-Link manuell an den Mieter
    (Saison 12 hat keinen automatischen Mail-Versand)
11. Mieter klickt den Link im Browser, ist eingeloggt, Browser
    haelt SSE-Verbindung zu `/m/events` und zeigt bei Klingel
    ein Bell-Overlay mit dem vom Admin vergebenen Mock-Namen

Die Mieter-CRUD-Seite (`/a/users`) ist eine bequeme Verwaltungs-
Ansicht der UA-User aus der Developer-API. Sie ist seit Saison
12-06-Refactor vollstaendig entkoppelt von Mock-Routing.

## 6. Lebenszyklus eines Klingel-Events (Saison-12-06-Endstand)

1. Besucher drueckt an echter UA Intercom den Klingel-Knopf
2. UDM publisht MQTT-RPC `/remote_view` an den im UA-Interface-
   Designer als Receiver konfigurierten Mock-Viewer
   (1:1-Routing ueber Receiver-Konfig; Frame-Inhalt enthaelt
   KEINE Mieter-ID, Identifikation ist implizit ueber
   "welcher Mock-Viewer empfaengt den Push")
3. Mock-Viewer-Goroutine empfaengt `/remote_view` in Stage 6
4. `handlers.Handler.RemoteView` persistiert `last_doorbell.json`
   UND ruft `OnDoorbell`-Callback auf
5. Library-`publishDoorbell` macht non-blocking Send auf den
   Per-Viewer-Channel `mock.Viewer.events`
6. Manager-Forwarder-Goroutine multiplexed in
   `mockmanager.Manager.eventCh`
7. `doorbellhub.Run` liest `event.MockMAC` und dispatched direkt
   an alle Subscribers fuer diesen Mock (keine Mieter-Aufloesung
   mehr, der Hub kennt nur Mock-MACs)
8. `handler_events.go` SSE-Loop schreibt
   `event: doorbell_start\ndata: <json>\n\n` zum Mieter-Browser
9. Browser-JavaScript zeigt das Bell-Overlay mit dem Mock-Namen
10. Mieter klickt "Tuer auf" - zwei Pfade (Saison 13-07):
    a. Bell-Overlay (waehrend einer aktiven Klingel): JS POSTet
       `POST /einloggen/doors/<intercom-mac>/unlock`. Die
       Intercom-MAC kommt aus dem SSE-doorbell_start.device_id-
       Frame. Server normalisiert auf colon-form.
    b. Standby (Schluessel-Knopf vom Idle-Screen): JS POSTet
       die literale URL `POST /einloggen/doors/standby/unlock`.
       Server liest `viewers.paired_intercom_mac` (Admin-Setting
       per "Verknuepfte Klingel"-Dropdown).
11. unifix-server resolved die Door-UUID via
    `uaapi.LookupDoorForIntercom(intercom-mac)`: iteriert ueber
    `GET /api/v1/developer/doors` und matched die MAC gegen
    `extras.door_thumbnail` (Pfad-Form
    `/preview/reader_<intercom-mac>_<door-uuid>_<ts>.jpg`).
12. unifix-server proxied via
    `PUT /api/v1/developer/doors/<door-uuid>/unlock` gegen die
    UniFi Access Developer-API mit Auth
    `Authorization: Bearer <token>`
13. UDM oeffnet via UA Hub Door die echte Tuer

Der Klingel-Cancel-Pfad ist analog: UDM publisht
`/cancel_doorbell_notification`, der Handler matcht das
Cancel-Token gegen den persistierten Eintrag und feuert den
`OnDoorbellCancel`-Callback. Der Hub schickt einen
`doorbell_cancel`-Frame zur gleichen Mieter-Session.

Reject- und End-Call-Pfad (Saison 13-04.5):

Wenn der Mieter "Ignorieren" oder "Anruf beenden" klickt
(/einloggen/reject bzw. /einloggen/end-call; /esp/reject
analog), passiert beim Server:

1. `doorbellcalls.MarkRejected` setzt `cancel_reason` in der
   doorbell_calls-Zeile (CAS, idempotenter Stale-Pfad).
2. Lokaler Cancel-Push via `eventBus.Publish(viewerMAC, ...)` -
   Geschwister-Sessions auf derselben MAC sehen den Cancel
   sofort und schliessen ihr Bell-Overlay.
3. `notifyUDMReject` -> `mockmanager.RejectDoorbellOnMock` ->
   `mock.Viewer.RejectDoorbell` -> Mock-Stage-6 publisht den
   89-Byte `/call_admin_result`-MQTT-RPC (Saison-13-04.5
   reverse-engineered).
4. UDM antwortet mit dem ueblichen
   `/cancel_doorbell_notification`-Broadcast an alle Receiver.
5. Die UA-Hardware-Intercom hoert SOFORT auf zu klingeln statt
   30 Sekunden auf den Hardware-Timeout zu warten.

Wire-Format-Details fuer `/call_admin_result` siehe
docs/wire-format.md Sektion "/call_admin_result body schema".

## 7. Saisons-Roadmap (Go-Aera)

Siehe CLAUDE.md Sektion 15 fuer die volle Detail-Tabelle. Kurz:

```
Saison 10:  Skelett + Server-Pool + Mock-Stages + Smoketest
            STATUS: abgeschlossen
Saison 11:  Klingel-Lifecycle komplett dekodiert
            Mock kann selbst Tueren oeffnen
            Mediaserver "ms" identifiziert (RTSPS 7441, LiveFLV 7550)
            Offizielle UniFi Access Developer-API entdeckt
            STATUS: abgeschlossen 12. Mai 2026
Saison 12:  Mieter-Plattform, Auth-Backbone, Mock-Embedding,
            Admin-UI, SSE-Klingel-Push, UA-Developer-API-Integration,
            mock-zentrisches Routing.
            STATUS: abgeschlossen 12. Mai 2026.
            Sub-Briefings:
   S12-01:  SQLite-Foundation + Magic-Link + Session            DONE
   S12-02:  TLS-HTTP-Server + Mieter-Login auf /m/              DONE
   S12-03:  Mock-Library + Mock-Manager + mock_viewers          DONE
   S12-DOC-01: Doku-Synchronisation nach S12-03                 DONE
   S12-04:  Admin-UI + UA-User-CRUD + secrets/platform_config   DONE
   S12-04-Hotfix: Authorization: Bearer, envelope.code string    DONE
   S12-05:  Klingel-Push-Pipeline Mock-Events -> SSE -> Browser DONE
   S12-05a: HTML-id-Konvention (macID-Helper, keine Doppelpunkte) DONE
   S12-05b: user_email-Korrektur + Magic-Link-Modal-Hotfix      DONE
   S12-06-Refactor: mock-zentrisches Routing (Migration 004)    DONE
   S12-DOC-02: Abschluss-Doku-Synchronisation                   DONE

Saison 13:  Sammelsaison mit fuenf Sub-Themen rund um Doorbell-
            History, UI-Politur und Stream. Keine reine Forschungs-
            Saison mehr.
            Sub-Briefings:
   S13-DOC-00: Roadmap-Fix (dieses Briefing).
   S13-01:  Doorbell-History.
            Migration 005 door_events-Tabelle, doorbellhub schreibt
            Events parallel zur Persistierung. Mieter-UI: Liste der
            letzten N Klingeln, Ungelesen-Indikator im Header.
            Admin-Dashboard: Klingel-Statistik. Hash-Chain optional
            (Vorbereitung fuer S16+ Stempelkarten).
   S13-02:  UI-Politur.
            Icon-Sizing-Konvention (Nav w-4 h-4, Card-Header w-6 h-6,
            Hero-Overlay w-24 h-24), allgemeiner Konsistenz-Pass,
            .gitattributes fuer CRLF-Drift.
   S13-03:  Tueroeffnen-Button + Anruf-Lifecycle-Forschung.
            "Tuer oeffnen" im Bell-Overlay (POST
            /api/v1/developer/doors/{id}/unlock via uaapi).
            Anruf-Lifecycle-RPCs erforschen (annehmen mit
            Gegensprechen, ablehnen ohne anzunehmen, beenden
            nach Annahme). Mieter-UI Anruf-Buttons funktional
            ohne Stream. Multi-Door wird hier NUR notiert, nicht
            implementiert (UA-Regel "jede Tuer einem Reader oder
            Intercom" wird erst angegangen wenn ein Kunde mehrere
            Tueren pro Intercom hat).
   S13-04:  Stream-Spike (Forschung).
            Drei Pfade pruefen:
            - Companion via Agora (S1-Befund: RemoteViewData
              hatte channel plus token)
            - UA-Intercom-Viewer via MQTT room_id (S11-Befund:
              field_9 = WR-<mac>-<id>)
            - Protect-URL via ms-Daemon (RTSPS 7441, LiveFLV 7550)
            tcpdump plus Wireshark plus offizielle Doku, Pcap
            einer echten Klingel-Annahme. Resultat: Architektur-
            Briefing fuer S13-05.
   S13-04.5-A/B: /call_admin_result + /input/state Wire-Format-
            Doku + Implementation. Mieter-Reject/EndCall pusht
            jetzt einen MQTT-RPC, sodass der Intercom sofort
            aufhoert zu klingeln statt 30s Hardware-Timeout
            abzuwarten.

   S13-05:  Door-Mapping-UI im Admin (SUPERSEDED durch S13-07).
            Admin-Page /a/intercom-mapping mit dropdown-basiertem
            intercom_to_door-Mapping in platform_config. Mieter-
            Unlock loeste die Intercom-MAC aus dem SSE-doorbell_
            start ueber dieses Mapping zur Door-UUID auf. Vier
            HOTFIX-Saisons fuer JSON-Schema-Drift, Intercom-Filter,
            Field-Names und MAC-Form-Normalisierung. Komplett
            geloescht in S13-07.

   S13-06:  Viewer-zu-Door Default-Mapping (SUPERSEDED durch
            S13-07). Zweite Tabelle in /a/intercom-mapping
            ("Viewer-Standby-Tuer") mit eigenem viewer_to_door-
            Key. Standby-Tuer-Knopf vom Mieter-Idle-Screen nutzte
            diesen Mapping.

   S13-07:  Aufraeumen - Auto-Door-Resolution. ABGESCHLOSSEN
            14. Mai 2026. UA-API liefert im /doors-Response ein
            extras.door_thumbnail-Feld dessen URL-Pfad
            "/preview/reader_<intercom-mac-hex>_<door-uuid>_
            <ts>.jpg" das intercom-zu-tuer-Mapping schon
            enthaelt; der admin-kuratierte Mapping-Aufwand aus
            S13-05/06 war ueberfluessig.
            Liefert:
              - uaapi.Door.IntercomMAC parst die Thumbnail-URL.
              - uaapi.LookupDoorForIntercom liefert die Door-
                UUID fuer eine Intercom-MAC.
              - Migration 011 fuegt viewers.paired_intercom_mac
                hinzu (eine MAC pro Viewer).
              - Web-Viewer Anlegen+Bearbeiten und ESP-Viewer
                Adoption bekommen ein "Verknuepfte Klingel"-
                Dropdown im Modal, gespeist via
                /a/intercoms.json.
              - handler_mieter_calls hat zwei Pfade: Bell-
                Overlay (intercom-MAC aus URL) und Standby
                (literal /einloggen/doors/standby/unlock,
                liest viewer.paired_intercom_mac).
            Geloescht: /a/intercom-mapping-Page, Klingel-Tuer-
            Nav-Link, platformconfig.intercom_to_door /
            viewer_to_door / KeyIntercomToDoor / KeyViewerToDoor.

   S13-08:  ESP-API Phase A. ABGESCHLOSSEN 14. Mai 2026.
            Liefert das Minimum damit der parallele ESP-Chat
            an der ESP32-P4-Firmware bauen kann:
              - POST /esp/reject - dedizierter Reject-Endpoint
                (doorbellcalls + sibling-cancel + UDM-Ring-Stop)
              - GET /esp/stream.mjpeg - Reverse-Proxy auf
                UNIFIX_STREAM_BACKEND_URL (503 wenn unkonfiguriert,
                Authorization-Header wird vor Forward gestrippt)
              - cmd/unifix-cli mit "esp adopt"-Subcommand:
                schreibt eine ESP-Viewer-Reihe mit frischem Bearer-
                Token, Klartext einmalig auf stdout
            Nutzt die existierende viewers-Tabelle (type='esp')
            + requireESPBearer-Middleware aus S12-S13; KEINE
            parallele esp_viewers-Tabelle und kein eigenes
            espstore-Paket wie urspruenglich im Briefing
            entworfen.

   S13-09:  Hybrid-Type-Fix fuer ESP-Klingel-Events.
            ABGESCHLOSSEN 14. Mai 2026. mockmanager.AddViewer
            spawnt jetzt die Mock-Goroutine fuer beide Typen
            ('web' und 'esp'); LoadFromDB liest type IN ('web',
            'esp') statt nur 'web'. Folge: ein adoptierter
            ESP-Eintrag wird im UDM als regulaerer UA-Int-Viewer
            adoptiert, /remote_view-RPCs landen am Mock-
            Goroutine, doorbellhub published auf eventbus mit
            ESP-MAC als Topic, /esp/events SSE liefert echte
            doorbell.ring-Frames an die ESP-Hardware. Plus:
            notifyUDMReject erreicht jetzt eine LAUFENDE Mock-
            Goroutine - Hardware-Klingel hoert via
            /call_admin_result-MQTT-RPC sofort auf. Hebt die
            S13-08-Notiz "Mock + ESP same-MAC nicht moeglich"
            auf.

   S13-DOC: Saison-13-Abschluss-Doku-Synchronisation.
            ABGESCHLOSSEN 14. Mai 2026. Bringt die fuenf Repo-
            Doks (CLAUDE.md, architecture, wire-format, security,
            feature-backlog) auf den Saison-13-Endstand. Kein
            Code, nur Konsolidierung. Loest gleichzeitig die
            Saison-14ff-Roadmap am 3-Stufen-Produktmodell aus
            (Stufe 1 self-hosted LAN, Stufe 2 Cloud-Bridge,
            Stufe 3 Premium UA-Stream).

Saison 14:  Stream-Integration plus Webhook-Endpoint. Live-View
            Video / Audio / Stumm-Button (urspruenglicher S13-05-
            Slot) wandern in S14-02+; S14-01-Block lieferte
            stattdessen den fest verkabelten MJPEG-Pfad.

   S14-01:  Stream-Backend go2rtc produktiv (16. Mai 2026,
            ABGESCHLOSSEN).
              - Migration 012 fuegt viewers.stream_profile
                hinzu (nullable TEXT, Convention-Fallback bei
                NULL: TypeWeb -> "intercom_browser",
                TypeESP -> "intercom_esp").
              - server/internal/streams kapselt den go2rtc-
                REST-API-Client (List, Get, Put, Delete).
              - /esp/stream.mjpeg und /einloggen/stream.mjpeg
                resolven das Profil ueber GetViewerInfo +
                ResolveStreamProfile, bauen die URL
                <UNIFIX_STREAM_BACKEND_URL>/api/stream.mjpeg
                ?src=<profile> und proxen Body mit Flush
                pro Read. Authorization-Header wird vor
                Forward gestrippt.
              - Admin-Page /a/streams mit Liste / Edit /
                Loeschen. POST-Save wirkt live ueber die
                go2rtc-REST-API; bestehende Konsumenten
                reconnecten beim naechsten Frame.
              - Web-Viewer- und ESP-Viewer-Modal bekommen
                ein Stream-Profil-Dropdown, gespeist via
                /a/streams.json.
              - Mieter-Klingel-Overlay rendert
                <img src="/einloggen/stream.mjpeg"> hinter
                dem Bell-Hero (object-fit: cover, opacity
                0.45). onerror blendet das Bild aus damit
                die Buttons bei Backend-Ausfall sichtbar
                bleiben.
              - go2rtc.yaml.example liegt im Repo-Root mit
                drei Default-Profilen (intercom_high als
                Source, intercom_esp und intercom_browser
                als FFmpeg-Derivate).
            Out-of-Scope (kommt in S14-02+):
              - Audio (Up- und Down-Stream)
              - WebRTC-Signaling (S14-02 oder spaeter)
              - Original-UA-Stream-Pfad (Premium, S20+)

   S14-01b: Idle-View-Modus mit Bildschirmschoner.
            ABGESCHLOSSEN 16. Mai 2026. Migration 013 fuegt
            viewers.idle_view_mode (NULL/'screensaver'/
            'livestream') hinzu plus station_lat/lon-Defaults
            in platform_config. Neues Paket internal/weather
            mit open-meteo-Client, 15-Min-Cache und
            24h-Stale-Serving. Mieter-Routes /einloggen/settings
            (idle-Default persistieren), /einloggen/weather
            (JSON fuer idle.js). Admin bekommt einen
            "Standort"-Block in /a/settings plus /a/weather
            als Preview. Mieter-home.html ist auf das
            screensaver/livestream-Container-Layout umgebaut;
            idle.js tickt die Uhr, refresht Wetter und
            toggelt per Tap. /esp/config bekommt
            idle_view_mode + weather als optionale Felder.

   S14-01-FIX01: Stream-Proxy URL-Hardening.
            ABGESCHLOSSEN 16. Mai 2026. Die String-
            Konkatenation aus S14-01-Block-A weicht einem
            dedizierten buildBackendStreamURL-Helper, der
            ueber net/url parsed, Path und Query explizit
            setzt, Trailing-Slashes + Fragments wegnimmt
            und einen Path-Prefix erhaelt. Plus strukturierte
            Per-Request-Logs (INFO stream proxy mit route,
            label, profile, backend, viewer_mac VOR dem
            Backend-Call) und WARN/ERROR/DEBUG fuer alle
            Fehler- + Disconnect-Pfade. main.go-Boot-Log
            liefert jetzt eine explizite "stream backend
            configured"-Zeile.

   S14-01-FIX02: ESP-Unlock-Auto-Door-Resolution.
            ABGESCHLOSSEN 16. Mai 2026. handleESPUnlock ist
            jetzt auf der Briefing-Spec: door_id und event_id
            sind beide optional; bei leerem door_id resolvt
            der Server ueber viewers.paired_intercom_mac +
            uaapi.LookupDoorForIntercom (S13-07-Pfad, gleich
            wie Mieter-Standby). Response-Envelope kriegt
            door_source=body|auto. Vier neue Tests in
            handler_esp_unlock_test.go decken die Pfade ab.

   S14-02:  Webhook-Endpoint.
            POST /webhook/access fuer access.doorbell.* und
            access.door.unlock-Events. Schreibt in die in
            S13-01 angelegte door_events-Tabelle. Event-Type-
            Dispatch-Pattern als Vorbereitung fuer S16+
            Plugins. HMAC-Signed-Body-Verifikation pro
            Webhook-Registration.

Saison 15:  ESP-Phase-B Plug-and-Play + /input/state-Audit.
            UDP-Discovery-Listener auf dem Server, der
            unbekannte ESPs in esp_pending_devices eintraegt;
            ESP haelt einen Long-Poll an
            /esp/wait-for-adoption offen, bekommt nach Admin-
            Klick auf /a/esp-viewers seinen Token zurueck.
            Plus optional: /input/state-Konsum fuer Tuer-
            Audit-Trail (door_opened/door_closed auch bei
            Schluessel-Oeffnung).

Saison 16:  Stempelkarten-Plugin + Stumm-Button-Variationen.
            time_clock_entries-Tabelle, UA-Standard-NFC-Hardware
            (Reader G2 / Pro / G3, Touch-Pass). Append-Only mit
            Hash-Chain. Plugin-Storage plugin_data. Eine
            NFC-Karte deckt Tuer plus Stempel plus Visitor ab.

Saison 17:  Production-Hardening + Lizenz-Server-Fleisch.
            Lizenz-Server-Schluessel-Generierung, Online-
            Validation, Update-Pakete-Verteilung. Hardware-
            Bindung Stufe A (RPi-Seriennummer-Pinning). TLS-
            Cert-Strategie (Self-Signed-Pinning -> Eigene CA
            via Lizenz-Server). Erst-Pilot-Anlagen-Setup.

Saison 18:  Eigene ESP32-Intercom-Hardware. ESP32-basiertes
            Intercom 200-300 EUR, eigene Adapter-Schicht unter
            internal/access/unifix/*. Nutzt die /esp/-API aus
            S13-08 als Wire-Format-Basis.

Saison 19:  Cloud-Bridge (3-Stufen-Modell Stufe 2). unifix-VPS,
            Authenticated Stream-Tunnel pro Klingel-Event,
            Mobile-App holt Stream via VPS. Skalierbar fuer
            viele Hausverwalter.

Saison 20:  Premium UA-Stream-Qualitaet (Stufe 3). Direkter
            Zugriff auf den ms-Mediaserver-Pfad (RTSPS 7441 /
            LiveFLV 7550), HD-Video plus Two-Way-Audio mit
            Echo-Cancellation.
```

## 8. Was unifix NICHT ist

- Kein UniFi-Replacement (UDM + Hub Door + echte Kamera bleiben Pflicht)
- Kein Open-Source-Projekt
- Kein Hardware-Hersteller (wir liefern Software, optional Convenience-RPi)
- Kein Sicherheits-Garantie-Geber (siehe security.md)
- Keine Cloud-Pflicht (alles lokal moeglich, Cloud-Update optional)

---

## 9. Plattform-Daten-Schicht (Saison 12)

unifix-server haelt eine eigene SQLite-Datenbank fuer Plattform-
Daten. UA-User-Stammdaten (Mieter-Identitaet, NFC-Karten, PIN-Codes,
Zutritts-Policies) bleiben in der UniFi Access Developer-API; die
Plattform-DB referenziert sie NICHT mehr als Foreign-Key (Saison
12-06-Refactor hat diese Annahme verworfen, weil ein Mock im
Lebenszyklus auch ohne UA-User existieren kann).

### 9.1 Treiber-Wahl

```
Treiber:        modernc.org/sqlite v1.50.1
                Pure-Go-Port von SQLite. Kein CGO, kompatibel
                mit CGO_ENABLED=0 (docs/security.md Sektion 4).
Pfad:           ./state/unifix.db  (Default, ueberschreibbar via
                UNIFIX_DB_PATH-env)
File-Mode:      0600 auf Linux, Windows ignoriert POSIX-Modes
Parent-Mode:    0700 (vom db.Open mkdir-Aufruf)
Journal:        WAL (gesetzt in db.Open via PRAGMA)
Foreign-Keys:   ON (gesetzt in db.Open via PRAGMA)
MaxOpenConns:   1 (sichert dass per-connection-Pragma stabil ist)
```

### 9.2 Migrations-Pattern

```
Verzeichnis:    server/internal/db/migrations/
Konvention:     NNN_kurzname.sql, NNN ist 3-stellige Nummer
Runner:         server/internal/db/migrate.go, eingebettet via
                //go:embed migrations/*.sql
Mechanismus:    Beim db.Open wird MAX(version) aus schema_version-
                Tabelle gelesen (oder 0 wenn Tabelle nicht
                existiert). Alle Files mit groesserer Nummer
                werden in alphabetischer Reihenfolge in je einer
                Transaktion ausgefuehrt. Jede Migration enthaelt
                ihren eigenen INSERT INTO schema_version (version,
                applied_at).
Idempotenz:     Ein zweites db.Open auf derselben DB tut keine
                Migrationen erneut (schema_version-Check verhindert
                Wiederholung).
Rollback:       Bei Fehler in einer Migration: tx.Rollback, db.Open
                gibt einen Fehler zurueck, Server startet nicht.
```

### 9.3 Tabellen-Inventar (Stand Migration 013)

> **Saison-13-Hinweis:** Migrations 005-011 haben das Schema
> deutlich erweitert (door_events, viewers-Rename + ESP-Felder,
> esp_pending_devices, doorbell_calls, paired_intercom_mac).
> Eine kompakte Delta-Uebersicht steht in Sektion 9.6 unten;
> der Vollinventar-Eintrag bleibt auf Saison-12-Endstand bis
> die naechste Schema-Saison ihn ueberarbeitet.

```
schema_version       Migrations-Tracking. PK version, applied_at.
                     Aktueller MAX(version) = 13 (Saison 14-01b:
                     viewers.idle_view_mode +
                     platform_config.station_lat/lon).
                     Migration 001.

magic_link_tokens    Magic-Link-Auth-Tokens.
                     PK token (Klartext, 43-char base64url),
                     mock_mac TEXT NOT NULL FK CASCADE auf
                     mock_viewers(mac), created_at, expires_at,
                     consumed_at (nullable).
                     Migration 004 (ersetzt Migration-001-Variante
                     mit ua_user_id).
                     Index idx_magic_link_mock.

mieter_sessions      Mieter-Browser-Sessions.
                     PK session_id (Klartext, 43-char base64url),
                     mock_mac TEXT NOT NULL FK CASCADE auf
                     mock_viewers(mac), created_at, last_seen,
                     expires_at, user_agent, ip.
                     Migration 004 (ersetzt die alte sessions-
                     Tabelle aus Migration 001).
                     Indexes idx_mieter_sessions_mock,
                     idx_mieter_sessions_expires.

mock_viewers         Persistierte Mock-Viewer-Configs.
                     PK mac (z.B. "0c:ea:14:42:42:42"), name
                     (Admin-vergeben), service_port UNIQUE,
                     created_at, updated_at.
                     KEIN ua_user_id mehr seit Migration 004.
                     Migration 002 angelegt, Migration 004 entfernt
                     die ua_user_id-Spalte (SQLite-Table-Recreate).
                     Index idx_mock_viewers_port (UNIQUE).

admin_users          Admin-Account-Stammdaten.
                     PK username, password_hash (bcrypt cost=12),
                     created_at, updated_at, last_login_at (nullable).
                     Migration 003. Saison 12 erlaubt genau einen
                     Admin-Account ueber den Erst-Setup-Flow auf
                     /a/login.

admin_sessions       Admin-Browser-Sessions.
                     PK session_id (43-char base64url),
                     admin_username TEXT NOT NULL FK CASCADE auf
                     admin_users(username), created_at, last_seen,
                     expires_at, user_agent, ip.
                     Migration 004 NEU (eigene Tabelle, vorher
                     waren Admin-Sessions in der gemeinsamen
                     sessions-Tabelle mit "_admin_<user>"-Prefix
                     als ua_user_id-Surrogat untergebracht).
                     Indexes idx_admin_sessions_user,
                     idx_admin_sessions_expires.

platform_config      Server-weite Key-Value-Settings.
                     PK key, value TEXT (Klartext, z.B. fuer
                     ua_api_base_url), value_encrypted TEXT
                     (AES-256-GCM, fuer ua_api_token), updated_at.
                     Pro Zeile ist entweder value oder
                     value_encrypted gesetzt, nie beide.
                     Migration 003.
```

### 9.4 Zukuenftige Tabellen-Roadmap

```
door_events          Klingel-Audit-Trail.
                     Wird in Saison 13-01 (Doorbell-History)
                     angelegt: der doorbellhub schreibt parallel
                     zur Persistierung, das Mieter-UI rendert die
                     letzten N Eintraege und einen Ungelesen-
                     Indikator. Saison 14 dockt zusaetzlich den
                     UA-Webhook-Endpoint an dieselbe Tabelle an
                     (Event-Type-Dispatch).
                     Felder: ts, mock_mac, action
                     ("doorbell", "unlock", "cancel", "reject"),
                     source ("ua", "tenant", "admin", "mock"),
                     request_id, raw_payload. Hash-Chain optional
                     als Vorbereitung fuer S16+ Stempelkarten.
                     Migration 005.

time_clock_entries   Stempelkarten-Plugin (Saison 16+). Felder:
                     ts, ua_user_id, kind ("in"/"out"/"break"),
                     location_id, source, hash_prev, hash_self.
                     Append-Only mit Hash-Chain.
                     Migration 006+.

plugin_data          Generic Plugin-Storage fuer Saison 16+
                     Plugin-System. Felder: plugin_id,
                     ua_user_id, key, value_json, created_at,
                     updated_at.
                     Migration 006+.
```

### 9.5 Foreign-Key-Konzept

```
Saison 12-06-Refactor hat den FK-Aufbau radikal vereinfacht:

mock_viewers.mac      Primaerschluessel und einziger Routing-
                      Schluessel der Plattform.

magic_link_tokens.mock_mac   ->  mock_viewers(mac) ON DELETE CASCADE
mieter_sessions.mock_mac     ->  mock_viewers(mac) ON DELETE CASCADE

admin_sessions.admin_username -> admin_users(username) ON DELETE CASCADE

ua_user_id-Felder kommen erst in den geplanten Saison-15+-
Tabellen door_events und time_clock_entries wieder vor, dort
als optionale Annotations-Referenz auf die UA-Welt (kein
echter FK weil UA der Source-of-Truth ist).
```

### 9.6 Schema-Delta seit Saison 12 (Migrations 005-011)

```
Migration 005 (S13-01)  door_events-Tabelle.
                        id PK AUTOINCREMENT,
                        viewer_mac TEXT FK CASCADE auf viewers,
                        intercom_mac TEXT,
                        event_type TEXT (z.B. "doorbell_received",
                                          "door_unlocked"),
                        occurred_at INTEGER (Unix-Millisekunden),
                        read_at INTEGER NULL (ungelesen wenn NULL).
                        Index nach (viewer_mac, occurred_at DESC)
                        plus partieller Index auf ungelesene Reihen.

Migration 006 (S13-02-FIX4-a)
                        mock_viewers -> viewers UMBENANNT.
                        viewers.type TEXT NOT NULL DEFAULT 'web'
                        (CHECK in 'web'|'esp').
                        viewers.password_hash + password_set_at +
                        esp_token_hash + esp_device_id +
                        esp_pending + esp_model + esp_fw_version +
                        linked_ua_user_id.
                        mieter_sessions -> viewer_sessions
                        UMBENANNT (Spalte mock_mac -> viewer_mac).
                        door_events.mock_mac -> viewer_mac.
                        magic_link_tokens DROP (kein Magic-Link
                        mehr - Username+Passwort statt).
                        login_audit-Tabelle NEU.

Migration 007 (S13-02)  viewer-username-Normalisierung.

Migration 008 (S13-02-FIX4-a-HOTFIX4)
                        viewers.username DROP - der Wohnungs-Name
                        IST der Login (case-insensitive Match).

Migration 009 (S13-02-FIX4-c)
                        esp_pending_devices NEU. PK mac, plus
                        model, fw_version, capabilities,
                        discovered_at, last_poll_at, rejected_at,
                        adopted_token_cleartext (einmalige
                        Klartext-Token-Auslieferung beim ersten
                        ESP-Status-Poll nach Adoption).

Migration 010 (S13-04.5-B)
                        doorbell_calls-Tabelle (Lifecycle-State
                        fuer den CAS-Style Answer/Reject-Arbiter).

Migration 011 (S13-07)  viewers.paired_intercom_mac TEXT NOT NULL
                        DEFAULT ''. Pro Viewer EINE Klingel als
                        Pairing fuer den Standby-"Tuer auf"-
                        Knopf (siehe Sektion 14 Auto-Door-
                        Resolution).

NICHT MEHR im Schema (S13-07 entfernt):
   platform_config.intercom_to_door
   platform_config.viewer_to_door
```


---

## 10. Mock-Viewer-Plattform-Architektur (Saison 12)

### 10.1 Architektur-Entscheidung

Mock-Viewer laufen als Goroutines IM unifix-server-Prozess, NICHT als
separate Prozesse. Beschluss Sascha 12. Mai 2026. Begruendung:

1. **Plattform-First-Architektur:** ein Binary fuer alles. Ein
   einzelner systemd-Service `unifix-server` startet und stoppt die
   gesamte Anlage. Erleichtert spaetere Updates ueber den
   Lizenz-Server, sobald der ausgebaut wird (zeitlich offen,
   ehemals Saison 14).
2. **Go-idiomatisch:** Goroutines sind ungefaehr 2 KB schwer. 50+
   Mock-Viewer pro RPi sind trivial; die Stage-Pakete halten
   intern ein paar TLS-Verbindungen und Listener, das ist der
   einzige nennenswerte Memory-Footprint.
3. **Simpelster Klingel-Push-Pfad ohne IPC:** Stage 6 MQTT-Handler
   schreibt direkt auf einen `chan DoorbellEvent`. Der Hub liest
   aus dem Channel. Kein RPC zwischen Prozessen, keine Socket-Datei,
   keine MessagePack-Serialisierung.

### 10.2 Library-API `unifix.local/mock`

```
mock.Config              Per-Viewer-Settings: MAC, IPv4, Name,
                         ServicePort, StateDir, GUID. Validate-
                         Methode prueft Pflichtfelder und Formate.

mock.Viewer              Hauptklasse. Konstruktor mock.New(cfg, log)
                         baut identity und reserviert die Channels.
                         Run(ctx) orchestriert Stages 1, 4, 5, 6 und
                         blockiert bis ctx-Done oder fataler Stage-
                         Fehler.

mock.DoorbellEvent       Library-Wire-Form eines Klingel-Pushes:
                         MockMAC, RequestID, DeviceID, RoomID,
                         CancelToken, CreateTimeUnix, ReceivedAt,
                         RawBody. Nicht zu verwechseln mit
                         handlers.DoorbellRecord (persistente
                         JSON-Form).

mock.DoorbellCancelEvent Library-Wire-Form eines Cancel-Pushes:
                         MockMAC, CancelToken, ReasonCode,
                         ReceivedAt.

mock.GenerateJWT         Helper fuer den standalone --show-jwt-Pfad.
```

### 10.3 Manager-Pattern (Saison-12-06-Endform)

`server/internal/mockmanager` ist die Goroutine-Ownership-Schicht.
Verantwortlichkeiten:

```
LoadFromDB(ctx)            Beim Server-Start: alle mock_viewers
                           lesen, je eine Viewer-Goroutine spawnen.

AddViewer(ctx, spec)       INSERT in mock_viewers, dann spawn.
                           Returnt ErrMACInUse / ErrPortInUse bei
                           Kollision mit bereits laufendem Viewer.

RemoveViewer(ctx, mac)     Cancel + Wait + DELETE. FK CASCADE
                           loescht assoziierte Mieter-Sessions und
                           ausstehende Magic-Link-Tokens
                           automatisch mit.

GetViewerInfo(ctx, mac)    Snapshot des Mock-Eintrags inkl. Name
                           und Service-Port fuer die Mieter-Home-
                           Seite und die Admin-Liste.

MockExists(ctx, mac)       Leichtgewichtiger Existenz-Check fuer
                           den Magic-Link-Generator.

ListViewers(ctx)           Snapshot aller laufenden Viewer fuer
                           das Admin-UI.

Events()                   Liefert den Multiplex-Channel aller
                           DoorbellEvents.

Cancels()                  Analog fuer DoorbellCancelEvents.

Shutdown(ctx)              Cancel't alle Viewer und wartet mit
                           Deadline.
```

Saison-12-06-Refactor: `UpdateUserBinding` und `LookupUserByMAC`
sind weg. Der Routing-Schluessel ist jetzt direkt die Mock-MAC.

### 10.4 Multiplex-Channel-Pattern

Jeder mock.Viewer hat eigene events- und cancels-Channels (16
gepuffert). Der mockmanager startet pro Viewer ZWEI Forwarder-
Goroutines die aus den Per-Viewer-Channels lesen und in zwei
gemultiplexte Manager-Channels schreiben (64 gepuffert). Vorteile:

- doorbellhub sieht nur EINEN Event- und EINEN Cancel-Channel.
- Stage-Goroutine blockiert nie an einem langsamen Consumer,
  weil sie non-blocking in den Per-Viewer-Channel schreibt.
- Forwarder kann auf eigene ctx-cancel reagieren wenn ein
  einzelner Viewer entfernt wird.

Drop-Verhalten bei voller Buffer: warn-Log mit MAC und RequestID.
Bei Saison-realistischen Klingel-Frequenzen (Sekunden bis Stunden
zwischen Klingeln) ist Drop nicht zu erwarten.

### 10.5 Test-Injektion via ViewerFactory

`mockmanager.Options.Factory` ist eine austauschbare ViewerFactory.
Production verwendet `DefaultFactory` (delegiert an mock.New).
Tests injecten einen FakeViewer / FakeFactory um Manager-Lifecycle
ohne echte Netzwerk-Sockets zu testen.

### 10.6 Standalone-Binary bleibt erhalten

`mock/cmd/mock/main.go` ist nicht weggeworfen. Es nutzt seit
S12-03 die selbe Library-API wie der Server, daher Verhalten
identisch. Wir behalten es fuer:

- Saison-Forschung: schnelle iterative Stage-Tests ohne Server-
  Komplexitaet.
- ESP-Saison-Wiederaufnahme (Saison 17+): das ESP-Geraet wird
  vermutlich aehnlich wie der Mock einen einzelnen Stage-Stack
  brauchen.
- Wire-Format-Verifikation gegen pcap-Goldminen.

---

## 11. Authentifikations-Schicht (Saison 12)

### 11.1 Mieter-Auth (Pfad `/m/`)

```
Mechanismus:    Magic-Link an einen Mock-Viewer gebunden (NICHT
                an einen UA-User). 32-Byte base64url-Token mit
                TTL nach Wahl des Generators (Admin-UI verteilt
                aktuell 24h-Tokens). Konsum tauscht Token gegen
                Session-Cookie.

Token-Service:  magiclink.Service mit Methoden
                Create(ctx, mockMAC) und
                CreateWithTTL(ctx, mockMAC, ttl).
                Consume(ctx, token) returnt die mock_mac fuer
                die Session-Erstellung. Token-Foreign-Key auf
                mock_viewers.mac mit ON DELETE CASCADE.

Cookie:         Name "unifix_m_session", Pfad "/m/", HttpOnly,
                SameSite=Strict, Secure (ausser DevMode).
                MaxAge 30d entsprechend Session-TTL.

Session:        43-char base64url-ID, gespeichert plain in der
                mieter_sessions-Tabelle. mock_mac-FK auf
                mock_viewers (CASCADE). Validate macht Rolling-
                Renewal (last_seen + expires_at = now + 30d) in
                einer Tx.

Login-Endpoint: GET /m/login?t=<token>.
                Optimistic Session-Check vor Token-Consume:
                eingeloggter Klient wird redirected ohne den
                Token zu verbrauchen.

Mieter-Home:    GET /m/ rendert die Browser-Seite mit Mock-Name
                (vom Admin vergeben) und SSE-Subscription auf
                /m/events.

Logout:         POST /m/logout, revokes Session und clear Cookie.
```

### 11.2 Admin-Auth (Pfad `/a/`, Saison 12-04)

```
Mechanismus:    Username + Passwort mit bcrypt (cost=12) in der
                admin_users-Tabelle. Erst-Setup-Flow auf
                /a/login fuer den allerersten Admin-Account;
                weitere Accounts derzeit nicht erlaubt.

Session-Tabelle: admin_sessions ist seit Saison 12-06 eine
                EIGENE Tabelle (Migration 004) mit FK auf
                admin_users(username). Vorher lagen Admin-
                Sessions in der gemeinsamen sessions-Tabelle
                mit "_admin_<user>"-Prefix als
                ua_user_id-Surrogat; das ist Geschichte.

Service:        adminsession.Service mit Methoden
                Create(ctx, username, meta),
                Validate(ctx, sessionID),
                Revoke(ctx, sessionID),
                CleanupExpired(ctx).
                DefaultIdleTimeout = 30 * 24 Stunden.

Cookie:         Name "unifix_a_session", Pfad "/a/". Sonst wie
                Mieter-Cookie (HttpOnly, SameSite=Strict, Secure
                ausser DevMode, MaxAge 30d).

Endpunkte:      /a/login (Erst-Setup oder normaler Login),
                /a/logout, /a/{$} (Dashboard), /a/settings,
                /a/mocks (Liste + Create + Delete +
                Magic-Link-Generator), /a/users (UA-User-CRUD
                via UA-API).
```

### 11.3 UA-Developer-API-Token

```
Auth:           Authorization: Bearer <token>
                X-API-KEY ist verworfen (S12-04-Hotfix):
                offizielle Doku v4.2.16 Sektion 2.7 dokumentiert
                ausschliesslich Bearer; X-API-KEY wird mit
                CODE_UNAUTHORIZED abgelehnt.

Storage:        AES-256-GCM-verschluesselt in der
                platform_config-Tabelle (Key "ua_api_token").
                Master-Key kommt aus der env-Variable
                UNIFIX_SECRETS_KEY (64 hex chars, 32 Bytes raw).
                cmd/genkey generiert frische Master-Keys. Nonce
                pro Wert (12 Bytes), als Praefix vom Ciphertext
                serialisiert.

Lebenszyklus:   Admin setzt einmalig pro Anlage in /a/settings
                eine Base-URL (Klartext in platform_config unter
                "ua_api_base_url") und einen API-Token. Aenderbar
                jederzeit. unifix-server baut den uaapi.Client
                lazy nach dem Setting-Save (kein Restart noetig,
                main.SetUAClient swappt zur Laufzeit).

Klartext-Logs:  Token NIE im Klartext loggen, max 8 Zeichen
                Praefix. Master-Key NIE im Log.

Key-Rotation:   Wechsel des UNIFIX_SECRETS_KEY macht alle
                verschluesselten platform_config-Werte
                ungueltig. Admin muss UA-Token im
                /a/settings neu eintragen. Master-Key-Verlust
                ist Operator-Verantwortung.
```

---

## 12. doorbellhub-Architektur (Saison 12-05, mock-mac-Endform 12-06)

```
Subscriber-Pattern: doorbellhub.Subscribe(mockMAC) liefert
   {Events: <-chan Event, MockMAC: mac} plus eine cleanup-
   Funktion. Per Mock-MAC koennen mehrere Subscriber registriert
   sein (mehrere Geraete im Haushalt). Alle bekommen denselben
   Push.

SSE-Endpoint:    GET /m/events (text/event-stream).
                 Mieter-Browser-EventSource verbindet sich
                 mit dem Cookie, requireSession liest die
                 mock_mac aus der Session und ruft
                 hub.Subscribe(mockMAC).

Frame-Format:    event: doorbell_start\ndata: <json>\n\n
                 bzw. event: doorbell_cancel\ndata: <json>\n\n
                 JSON-Felder: type, mock_mac, request_id,
                 device_id, room_id, cancel_token, created_at
                 (Unix-Millisekunden).

Heartbeat:       :keepalive\n\n alle 30 Sekunden.
                 Testbar via Deps.EventsHeartbeat
                 (im Test-Setup z.B. 50 ms).

Browser-Verhalten: EventSource macht automatisches Reconnect
                 wenn die Verbindung abbricht; kein eigener
                 Timer im Mieter-JavaScript noetig.

Subscriber-Buffer: 8 Events. Bei Overflow drop mit WARN-Log
                 (Mock-MAC plus RequestID). Saison-realistisch
                 fast nie ein Issue.

Dispatching:     hub.Run liest aus mockmanager.Events() und
                 mockmanager.Cancels(), schreibt event.MockMAC
                 als Routing-Schluessel und fan-out an alle
                 Subscribers fuer diesen Mock. Keine
                 UA-User-Aufloesung mehr (S12-06-Refactor).

Stats:           hub.Stats() liefert SubscriberCount,
                 UniqueMockCount, EventsTotal, EventsDropped
                 (fuer das Admin-Dashboard oder Tests).
```

---

## 13. Admin-UI-Architektur (Saison 12-04 ff.)

```
Stack:           Go html/template + Tailwind-CDN + htmx
                 + Lucide-Icons via go:embed (Saison-10-Beschluss
                 "Weg A"). Keine Framework-Dependencies, keine
                 Single-Page-App, kein Webpack.

Layout:          Dark Linear-/Vercel-aesthetisch, max-width
                 Container, Sidebar-Navigation (Dashboard,
                 Mock-Viewer, Mieter, Einstellungen).

HTML-id-Konvention: Doppelpunkte vermeiden weil
                 querySelectorAll sie als CSS-Pseudoklasse
                 interpretiert. Template-Helper macID rendert
                 z.B. mock-row-0cea140a7806 (lowercase Hex
                 ohne Trennzeichen). URL-Pfade behalten die
                 MAC-Form mit Doppelpunkten
                 (DELETE /a/mocks/0c:ea:14:.../).

Magic-Link-Modal: Admin klickt "Login-Link" auf einer
                 Mock-Zeile, htmx-POST schickt
                 POST /a/mocks/{mac}/magic-link, Server
                 rendert das Modal-Partial in #modal-slot.
                 Modal enthaelt clickable-readonly
                 Magic-Link-URL plus Copy-Button.

Routes:          GET /a/login              Setup oder Login
                 POST /a/login             Setup oder Login
                 POST /a/logout
                 GET /a/{$}                Dashboard
                 GET /a/settings           UA-API-Settings
                 POST /a/settings
                 GET /a/mocks              Mock-Liste
                 POST /a/mocks             Create
                 DELETE /a/mocks/{mac}
                 POST /a/mocks/{mac}/magic-link
                 GET /a/users              UA-User-Liste
                 POST /a/users             Create
                 DELETE /a/users/{id}

Hinweis:         Die Mieter-/Users-Page ist seit S12-06 rein
                 zur Verwaltung der UA-User. Sie ist NICHT mit
                 den Mock-Viewern verkoppelt; ein Mock-Viewer
                 hat keine UA-User-Annotation in der DB.
```


---

## 14. Auto-Door-Resolution (Saison 13-07)

Vor S13-07 hatte unifix ein admin-kuratiertes Mapping von
intercom-MAC zu door-UUID in der `platform_config`-Tabelle. Bei
jeder neuen Klingel im Hauseingang musste der Admin manuell den
Eintrag pflegen.

S13-07 hat die Beobachtung umgesetzt: die UA-Door-Antwort enthaelt
das Feld `extras.door_thumbnail` mit einem URL-Pfad der Form

```
/preview/reader_<intercom-mac-12hex>_<door-uuid>_<ts>.jpg
```

Das ist die Verknuepfung intercom-zu-tuer in der UA-Antwort selbst.
unifix iteriert ueber `ListDoors()`, parst die Thumbnail-URL und
laesst das admin-kuratierte Mapping komplett weg.

```
uaapi.Door.IntercomMAC()         Parser fuer den Thumbnail-Pfad,
                                  liefert die colon-form lowercase
                                  intercom-MAC oder leeren String
                                  bei kein-Mapping.

uaapi.LookupDoorForIntercom(mac) ListDoors() iterieren, erste Door
                                  zurueckliefern deren IntercomMAC()
                                  matcht. Leerer String und nil
                                  Error bei kein-Match.
```

Konsequenz fuer den Admin: KEINE manuelle Mapping-Eingabe noetig.
Pro Viewer wird im Admin-UI nur `paired_intercom_mac` gesetzt
(welche Klingel ist mit diesem Viewer verknuepft); die Tuer kommt
automatisch aus der UA-Antwort.

Geloescht in S13-07: `/a/intercom-mapping`-Page + Handler,
`platformconfig.intercom_to_door` + `viewer_to_door`,
`KeyIntercomToDoor` + `KeyViewerToDoor`-Konstanten, Klingel-
Tuer-Nav-Link.

---

## 15. ESP-API (Saison 13-08)

### 15.1 Zweck

Eigener API-Pfad fuer ESP-Endgeraete getrennt vom Mieter-Web-API.
Ein ESP-Endgeraet wird vom unifix-Server adoptiert wie ein
Mock-Viewer (eigene Zeile in `viewers` mit `type=esp`). Bearer-
Token-Auth, eigene Middleware (`requireESPBearer`), eigene
Endpoint-Familie unter `/esp/`.

Strategische Klaerung Saison 13-08: ein ESP ist KEIN Mieter,
sondern ein Endgeraet eines Mieters. Es lebt im /esp/-Tree mit
geraete-skoptem Bearer-Token, NICHT im /einloggen/-Tree mit
Magic-Link-Session-Cookie.

### 15.2 Adoptions-Modell

```
Phase A (Saison 13-08, fertig):
  Workflow 1 (Discover-First):
    - ESP sendet POST /esp/discover (im LAN, ohne Auth)
    - unifix-Server traegt in esp_pending_devices ein
    - Admin klickt im /a/esp-viewers auf "Adoptieren"
    - Server generiert frischen 32-Byte-Bearer-Token
    - ESP holt den Token via GET /esp/discover/status
      (Long-Poll, einmalige Klartext-Auslieferung)
  Workflow 2 (CLI-First, headless):
    - Operator: unifix-cli esp adopt --mac ... --name ...
                                     [--intercom <mac>]
                                     [--mieter <ua-user-id>]
    - CLI generiert Bearer-Token + INSERT in viewers
    - Klartext-Token einmal auf stdout
    - Token wird per esptool / Setup-UI im NVS des ESPs
      gespeichert

Phase B (Saison 15 geplant):
  - UDP-Discovery-Listener auf dem Server (analog zu UDM
    selbst)
  - ESP haelt HTTP-Long-Poll an /esp/wait-for-adoption offen
  - Bei Admin-Klick: Token wird via Long-Poll geliefert
  - Vollstaendig zero-touch fuer den Operator
```

### 15.3 Endpoint-Inventar

```
GET  /esp/heartbeat        Liveness-Check (saison 13-02-FIX4-d)
GET  /esp/config           Mieter-Name, Stream, UI-Hints
GET  /esp/events           SSE-Stream mit doorbell.ring/cancel
POST /esp/answer           Anruf annehmen + Sibling-Cancel
POST /esp/reject           Anruf ablehnen (S13-08, dedicated)
POST /esp/unlock           Tuer auf via paired_intercom_mac
POST /esp/state            ESP-side status report (UI-Snapshot)
GET  /esp/stream.mjpeg     MJPEG-Reverse-Proxy (S13-08)

AUTH
  Authorization: Bearer <token>
  Server-Lookup via SHA-256-Hash in viewers.esp_token_hash
  (type=esp-Filter; revoked Tokens = 401).
```

Wire-Format-Details fuer alle Endpoints siehe
`docs/wire-format.md` Sektion "ESP-API Wire-Format".

### 15.4 Admin-UI / CLI-Tools

```
/a/esp-viewers       Pending-Liste (Phase A Discover-First)
                     plus adoptierte ESPs.
                     Adopt-Modal mit Name, paired-intercom-
                     dropdown, optionale UA-User-Verknuepfung.
                     "Token erneuern" + "Loeschen"-Aktionen.

unifix-cli           Headless-Tool fuer CLI-First-Adoption.
   esp adopt         Generiert Token + INSERT viewers + stdout.
   --mac <MAC>       Pflicht.
   --name <NAME>     Pflicht (max 64 chars).
   --intercom <MAC>  Optional - wird in paired_intercom_mac
                     gesetzt. Ohne diesen Wert liefert
                     /esp/unlock 400 "no paired intercom
                     configured" (Saison-14-01-FIX02-Spec,
                     loesste die buggy 404-Notiz aus S13-08 ab).
   --mieter <UA-ID>  Optional - in linked_ua_user_id; rein
                     Annotation, kein Routing-Effekt.
   --db <PATH>       Optional - default ./state/unifix.db.
```

### 15.5 Stream-Backend-Reverse-Proxy

`/esp/stream.mjpeg` ist ein Profile-bewusster MJPEG-Proxy auf
`UNIFIX_STREAM_BACKEND_URL`. Der Authorization-Header wird vor
dem Forward gestrippt (das Backend ist typisch ein lokaler
go2rtc-Daemon ohne Auth; ESP-Token darf nie ueber den unifix-
Prozess hinaus). Wenn die Env-Variable nicht gesetzt ist:
HTTP 503 "stream backend not configured". Volle Spezifikation
in Sektion 16 weiter unten (Saison 14-01: go2rtc-Profile +
S14-01-FIX01-URL-Hardening).

---

## 16. Stream-Backend (Saison 14-01, go2rtc)

unifix nutzt go2rtc als Stream-Bridge. UDM (UniFi Protect) liefert
RTSPS auf Port 7441, go2rtc transmuxt das fuer alle weiteren
Klienten in beliebigen Formaten (MJPEG fuer ESP und Browser; HLS
und WebRTC sind fuer spaetere Saisons vorbereitet aber noch nicht
verkabelt). Saison 14-01-FIX01 hat den URL-Bau gehaertet (siehe
16.4) und strukturiertes Logging eingefuehrt.

### 16.1 Daten-Pfad

```
UA Intercom Hardware
    | RTSPS:7441
    v
go2rtc-Daemon (RPi, localhost:1984)
    +-- intercom_high     (Source-Profil, RTSPS)
    +-- intercom_esp      (ffmpeg:intercom_high#video=mjpeg, 9 FPS)
    +-- intercom_browser  (ffmpeg:intercom_high#video=mjpeg, 12 FPS)
    | HTTP MJPEG
    v
unifix-server (Pass-through-Proxy mit Flush pro Read)
    +-- /esp/stream.mjpeg          (Bearer-Auth, ESP-Tier)
    +-- /einloggen/stream.mjpeg    (Session-Auth, Mieter-Tier)
    | HTTPS multipart/x-mixed-replace
    v
Endgeraet (ESP32-P4-Display, Mieter-Browser-img)
```

### 16.2 Profile-Resolution

Jeder Viewer hat eine `stream_profile`-Spalte (Migration 012,
nullable TEXT). Resolution-Order in `ViewerInfo.ResolveStreamProfile`:

1. Explizit gesetztes `stream_profile`
2. `type='esp'` -> `intercom_esp`
3. `type='web'` -> `intercom_browser`
4. Defensive Fallback `intercom_default`

Die Convention-Defaults sind in lock-step mit der
`go2rtc.yaml.example`-Vorlage. Wird ein Convention-Profil in go2rtc
umbenannt OHNE den unifix-Code mitzubewegen, zeigen neue Viewer auf
ein nicht existentes Source - die Admin-UI raeumt das auf, sobald
ein konkretes Profil per Dropdown gewaehlt wird.

### 16.3 Admin-Profile-CRUD

```
Route                                Wirkung
GET    /a/streams                    HTML-Liste plus Create-Modal
GET    /a/streams.json               Profile-Array fuer Viewer-Dropdown
POST   /a/streams                    Create (form: name, source)
GET    /a/streams/{name}             Edit-View (Source bearbeiten)
POST   /a/streams/{name}             Update (form: source)
POST   /a/streams/{name}/delete      Loeschen (form)
DELETE /a/streams/{name}             Loeschen (JSON / REST)
```

Vorlage-Buttons im Create-Modal fuellen Name und Source mit den
drei Convention-Profilen. Beim Speichern wirkt die Aenderung live
ueber den go2rtc-PUT-Call; bestehende Konsumenten reconnecten beim
naechsten Frame (1-3 Sek Aussetzer).

### 16.4 Stream-Proxy-Mechanik

`proxyMJPEGStream` (httpserver/handler_esp_stream.go) ist der
gemeinsame Kern fuer ESP- und Mieter-Pfad. Wesentliche Punkte:

- Same-Context GET gegen
  `<UNIFIX_STREAM_BACKEND_URL>/api/stream.mjpeg?src=<profile>`.
- Saison 14-01-FIX01: URL-Bau via `buildBackendStreamURL`-Helper
  ueber `net/url` (Path und Query explizit, keine String-
  Konkatenation). Tolerant gegenueber Trailing-Slashes und
  Fragments; ein konfigurierter Path-Prefix bleibt erhalten.
- Authorization-Header wird NICHT geforwarded (vermeidet, dass
  der ESP-Bearer den unifix-Prozess verlaesst).
- Response-Header werden 1:1 durchgereicht (Content-Type ist
  typisch `multipart/x-mixed-replace;boundary=frame`).
- Body wird mit 32-KB-Buffer gelesen, jeder Chunk sofort via
  `http.Flusher.Flush` rausgeschoben. KEIN `io.Copy`, KEIN bufio.
- Client-Disconnect terminiert die Goroutine ueber `r.Context()`.
- Pro Request schreibt der Proxy eine strukturierte INFO-Zeile
  (route + label + profile + backend + viewer_mac) VOR dem
  Backend-Call; Fehler-Pfade liefern eigene WARN/ERROR-Zeilen
  mit gleichen Schluesseln plus `bytes_streamed`-Counter bei
  Client- oder Backend-Disconnect.

### 16.5 Konfigurations-Vertrag

```
UNIFIX_STREAM_BACKEND_URL  go2rtc Base-URL ohne Pfad-Suffix.
                            Beispiel: http://127.0.0.1:1984
                            Empty: Server startet weiter, Stream-
                            Endpoints liefern 503, /a/streams
                            rendert "go2rtc nicht konfiguriert".
                            Production sollte das Env-Var setzen;
                            in S17+ wandert die Pruefung in
                            config.Validate().
```

Die `streams.Client`-Instanz wird in `main.go` einmalig beim Boot
gebaut. Hot-Reload bei Env-Var-Aenderung gibt es bewusst nicht;
Operator startet `unifix-server` neu wenn er die go2rtc-Adresse
aendert (Production: `systemctl restart unifix-server`).

## 17. Idle-View-Modus + Wetter-Backend (Saison 14-01b)

Der Mieter waehlt seinen Default-Idle-Modus zwischen
Bildschirmschoner (Uhrzeit + Datum + Wetter) und Live-Ansicht
(direkter MJPEG-Stream). Tap auf den Container toggelt
temporaer; Reload kehrt zum User-Default zurueck.

### 17.1 Container-Architektur

```
GET /einloggen/                    (requireSession)
  -> handler_home.handleHome
     -> mockmanager.GetViewerInfo(mac)
     -> info.ResolveIdleViewMode()      ("screensaver"|"livestream")
     -> server.fetchHomeWeather(r)      (*weather.Snapshot or nil)
     -> renderViewer("home", viewerHomeData{...})

home.html rendert:
  <div id="idle-container" data-default-mode="{{.IdleViewMode}}">
     <div id="screensaver" class="idle-view ...">    clock+date+weather
     <div id="livestream" class="idle-view ...">     <img src="..stream..">
  </div>
  {{template "intercom-ringing.html" .}}             klingel-overlay
```

Initial-Sichtbarkeit kommt vom Server: `idle-hidden`-Klasse haengt
am Container der NICHT dem Default-Mode entspricht, damit kein
Flash beim Render. Browser-Side macht `idle.js` Clock-Tick,
Weather-Refresh und Tap-Toggle.

### 17.2 Open-Meteo-Backend

```
internal/weather/openmeteo.go   Client mit Get(ctx, lat, lon)
internal/weather/cache.go        15-Min-Fresh + 24h-Stale-Serving
internal/weather/wmo_codes.go    WMO-Code -> Lucide-Icon + Beschreibung

API:   https://api.open-meteo.com/v1/forecast
         ?latitude=<lat>&longitude=<lon>
         &current=temperature_2m,weather_code
         &timezone=Europe/Berlin

Auth:  keine (kein API-Key, kein Rate-Limit)
```

Cache-Verhalten:

- `Get` -> `fresh()`: wenn juenger als 15 Min, sofort zurueck.
- Sonst Live-Call. Erfolg -> in Cache schreiben + zurueck.
- Live-Call-Fehler -> `stale()`: wenn juenger als 24h, stale
  Snapshot zurueck (Browser merkt nichts).
- Sonst `ErrUnavailable`. Mieter-UI blendet den Wetter-Bereich
  aus, ESP-Config laesst das `weather`-Feld weg.

Eine Anlage = ein Standort = ein Cache-Slot. Mehrere Mieter
desselben Mock-Viewers teilen sich den gecachten Wert.

### 17.3 Standort-Konfiguration

```
platform_config:
   station_lat   "51.6144"    Default Recklinghausen (Migration 013)
   station_lon   "7.1959"     Default Recklinghausen (Migration 013)

Admin-UI:        /a/settings, Section "Standort fuer Wetter-Anzeige"
Validierung:     Float-Parse plus Range-Check (lat in [-90,90],
                 lon in [-180,180]). Komma wird zu Punkt
                 normalisiert.
Wirkung:         beim naechsten 15-Min-Cache-Tick (existing
                 entry bleibt bis dahin gueltig).
Preview:         /a/weather liefert das aktuelle JSON live.
```

### 17.4 Mieter-Routen

```
GET  /einloggen/settings     Settings-Form (radio idle_view_mode +
                              Logout-Knopf)
POST /einloggen/settings     mockmanager.SetIdleViewMode +
                              Redirect /einloggen/
GET  /einloggen/weather      JSON-Snapshot fuer idle.js-Refresh
GET  /einloggen/             Home-Page mit idle-container
```

Settings-Link rendert als kleines Zahnrad-Icon oben rechts in der
home-Page (z-index 50, blur-Backdrop).

### 17.5 ESP-Integration

Der `/esp/config`-Response bekommt zwei neue Felder:

```json
{
  "idle_view_mode": "screensaver",
  "weather": {
    "temp_c": 11.4,
    "weather_code": 3,
    "description": "Bewoelkt",
    "icon": "cloud",
    "fetched_at": "2026-05-16T13:42:18Z"
  }
}
```

`weather` ist `omitempty`: wenn der open-meteo-Cache leer und
das Backend nicht erreichbar ist, fehlt das Feld komplett. ESP-
Saison-3 entscheidet selbst ob er den Snapshot aus `/esp/config`
verwendet oder eigene Polls macht.

### 17.6 Konfigurations-Vertrag

Keine neuen Env-Vars. `weather.Client` wird unbedingt in
`main.go` instanziiert; bei Internet-Outage liefert das Backend
einfach `ErrUnavailable` und die UI degradiert sauber. Operator
muss nichts setzen, kein API-Key, keine Konfiguration ausserhalb
des Admin-UI.
