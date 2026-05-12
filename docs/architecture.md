# unifix Architecture

**Status:** Saison 12 abgeschlossen 12. Mai 2026 (S12-DOC-02).
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

Lizenz-Server (Saison 14+):
   Eine Cloud-Instanz, validiert Lizenzschluessel ALLER Kunden,
   verteilt Updates und CA-Cert-Bundles
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
10. Mieter klickt "Tuer auf" (Saison 14: Button noch nicht live)
11. Browser sendet POST `/m/doors/<id>/unlock` an unifix-server
12. unifix-server proxied via
    `PUT /api/v1/developer/doors/<id>/unlock` gegen die UniFi
    Access Developer-API mit Auth `Authorization: Bearer <token>`
13. UDM oeffnet via UA Hub Door die echte Tuer

Der Klingel-Cancel-Pfad ist analog: UDM publisht
`/cancel_doorbell_notification`, der Handler matcht das
Cancel-Token gegen den persistierten Eintrag und feuert den
`OnDoorbellCancel`-Callback. Der Hub schickt einen
`doorbell_cancel`-Frame zur gleichen Mieter-Session.

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

Saison 13:  Stream-Spike (Forschungs-Saison).
            Drei Pfade fuer Live-View prototypisieren:
            - Companion-Webhook plus Agora (UA-empfohlener Pfad)
            - UA-Intercom-Viewer/MQTT-Mock mit room_id-Capture
            - Protect-URL gegen den ms-Mediaserver (7441 oder 7550)
            Entscheidung am Saison-Ende welcher Pfad in S14
            fest implementiert wird.

Saison 14:  Live-View-Implementation.
            Video plus Audio im Mieter-Browser, Tueroeffnen-
            Button (PUT /api/v1/developer/doors/:id/unlock),
            Hang-up-Button mit sauberem track.stop + pc.close.

Saison 15:  Webhook-Endpoint + Klingel-History-Tabelle.
            POST /webhook/access fuer access.doorbell.* und
            access.door.unlock-Events, door_events-Tabelle
            (Migration 005). Event-Type-Dispatch als Vorbereitung
            fuer S16+ Plugins.

Saison 16:  Design-Politur + Stempelkarten-Plugin.
            time_clock_entries-Tabelle, UA-Standard-NFC-Hardware
            (NFC-Reader an Hub Door). Append-Only mit Hash-Chain
            vorbereitet. Plugin-Storage-Tabelle plugin_data
            (Migration 006+).

Saison 17+: Eigene Intercom-Hardware auf ESP32-Basis,
            Lizenz-Server-Fleisch, Production-Hardening,
            erste Pilot-Anlage. Open-Source-Strategie-Review offen.
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

### 9.3 Tabellen-Inventar (Stand Migration 004)

```
schema_version       Migrations-Tracking. PK version, applied_at.
                     Aktueller MAX(version) = 4.
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
door_events          Audit-Trail fuer Webhook-Empfang in Saison 15
                     (verschoben aus dem urspruenglichen S12-07-
                     Plan). Felder: ts, mock_mac, action
                     ("doorbell", "unlock", "cancel", "reject"),
                     source ("ua", "tenant", "admin"), request_id,
                     raw_payload. Hash-Chain optional fuer
                     Append-Only-Garantie.
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

---

## 10. Mock-Viewer-Plattform-Architektur (Saison 12)

### 10.1 Architektur-Entscheidung

Mock-Viewer laufen als Goroutines IM unifix-server-Prozess, NICHT als
separate Prozesse. Beschluss Sascha 12. Mai 2026. Begruendung:

1. **Plattform-First-Architektur:** ein Binary fuer alles. Ein
   einzelner systemd-Service `unifix-server` startet und stoppt die
   gesamte Anlage. Erleichtert Updates ueber den Lizenz-Server in
   Saison 14.
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
