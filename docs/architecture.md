# unifix Architecture

**Status:** Saison 12 (Stand 12-03), lebendes Dokument, wird pro Saison ergaenzt.
**Geltungsbereich:** Interne Architektur-Entscheidungen, strategische
Eckpunkte. KEIN Marketing-Material, KEIN Open-Source-Hinweis.

## 1. Drei-Schichten-Plattform

```
UniFi-Welt (UDM + Hub Door + echte Intercom) <- bleibt unveraendert
   |
   | MQTT/mTLS, WebSocket/JWT, HTTPS-Adoption
   v
unifix-Host (RPi pro Standort)
   - mock-Daemons (Go-Subprozesse, simulieren UA Intercom Viewer)
   - unifix-server (REST-API, UI, Pool-Manager, Persistenz)
   - go2rtc (Stream-Bridge fuer RTSP-zu-Klient)
   |
   | HTTP/HTTPS, Magic-Link, Long-Poll-Events
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
   N x Mock-Geraete (Go-Subprozesse vom Pool-Manager gespawnt)
   N x Mieter-Endgeraete (Browser, ESP, etc.)

Pro Sascha-Kunde (Multi-Anlage):
   Mehrere RPis, jeweils einer pro Anlage
   Optional zentraler Verwaltungs-Cockpit (spaeter)

Lizenz-Server (Saison 14+):
   Eine Cloud-Instanz, validiert Lizenzschluessel ALLER Kunden,
   verteilt Updates und CA-Cert-Bundles
```

## 5. Lebenszyklus eines Mock-Viewers (Saison 12 Form)

Saison-10-Vision sah hier einen Pool-Manager mit Subprozessen vor.
Diese ist durch die Saison-12-Architektur-Entscheidung abgeloest:
Mock-Viewer laufen als Goroutines im unifix-server-Prozess. Siehe
Sektion 10 fuer die Begruendung.

Aktueller Lebenszyklus eines Mock-Viewers:

1. Admin im unifix-server-Admin-UI (S12-04): "Mock-Viewer anlegen"
2. mockmanager.AddViewer persistiert in mock_viewers-Tabelle
3. mockmanager spawnt mock.Viewer-Goroutine mit Stage 1 + 4
4. Mock-Viewer antwortet auf UDM-Multicast-Discovery (Stage 1)
5. Admin im UA-Interface-Designer: "Verwenden"
6. UDM schickt Adoption-Push an Mock:port (Stage 4)
7. mock.Viewer baut WS (Stage 5) und MQTT (Stage 6) auf
8. UDM sieht Mock als online (gruener Punkt)
9. Admin im UA-Interface-Designer: Mock einem Mieter (UA-User)
   als Receiver fuer eine Klingel zuordnen
10. Admin im unifix-server-Admin-UI:
    mockmanager.UpdateUserBinding(mac, ua_user_id) verknuepft den
    Mock mit dem Mieter fuer Plattform-Zwecke (Magic-Link-Versand,
    Browser-Session-Routing). KEIN Restart des Viewers noetig.
11. Admin sendet Mieter den Magic-Link (S12-04 wird das automatisieren)

## 6. Lebenszyklus eines Klingel-Events

1. Besucher drueckt an echter UA Intercom den Klingel-Knopf
2. UDM publisht MQTT-RPC /remote_view an den im UA-Interface-Designer
   verknuepften Mock-Viewer (1:1-Routing ueber Receiver-Konfig,
   Frame-Inhalt enthaelt KEINE Mieter-ID, Identifikation ist
   implizit ueber "welcher Mock-Viewer empfaengt den Push")
3. Mock-Viewer-Goroutine empfaengt /remote_view in Stage 6
4. handlers.Handler.RemoteView persistiert last_doorbell.json UND
   ruft OnDoorbell-Callback auf
5. Library-publishDoorbell macht non-blocking Send auf den
   Per-Viewer-Channel mock.Viewer.events
6. Manager-Forwarder-Goroutine multiplexed in
   mockmanager.Manager.eventCh
7. S12-05 Browser-Push-Hub (geplant) liest aus dem Multiplex-
   Channel, ruft mockmanager.LookupUserByMAC fuer die
   ua_user_id-Aufloesung, schickt SSE- oder WebSocket-Frame an
   die Mieter-Browser-Session
8. Mieter sieht Live-Bild, klickt "Tuer auf"
9. Browser sendet POST /m/doors/<id>/unlock an unifix-server
10. unifix-server (S12-06) proxied via
    PUT /api/v1/developer/doors/<id>/unlock gegen die UniFi
    Access Developer-API auf der UDM (Alternative: Mock-RPC
    fuer Test-Setups ohne offizielle API)
11. UDM oeffnet via UA Hub Door die echte Tuer

Der Klingel-Cancel-Pfad ist analog: UDM publisht
/cancel_doorbell_notification, der Handler matcht das Cancel-Token
gegen den persistierten Eintrag und feuert
OnDoorbellCancel-Callback. Der Hub schickt einen cancel-Frame
zur gleichen Browser-Session.

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
Saison 12:  Mieter-Plattform, Auth-Backbone, Mock-Embedding.
            STRATEGISCHE MAXIME: maximal Original-API uebernehmen
            (UniFi Access Developer API v4.2.16 als Hauptquelle)
            Detail-Status der Sub-Briefings:
   S12-01:  SQLite-Foundation + Magic-Link + Session
            STATUS: abgeschlossen
   S12-02:  TLS-HTTP-Server + Mieter-Login auf /m/
            STATUS: abgeschlossen
   S12-03:  Mock-Library + Mock-Manager + mock_viewers
            STATUS: abgeschlossen, live-verifiziert 12. Mai 2026
   S12-04:  Admin-UI Skelett mit Mock-Viewer-CRUD (Pfad /a/)
            STATUS: geplant
   S12-05:  Klingel-Push-Pipeline (Mock-Events -> SSE -> Browser)
            STATUS: geplant
   S12-06:  Mieter-UI mit Live-Klingel-View
            STATUS: geplant
   S12-07:  Webhook-Endpoint (Audit + Stempelkarten-Vorbereitung)
            STATUS: geplant
   S12-08:  RTSPS-Stream-Spike auf Port 7441
            STATUS: geplant
Saison 13:  ESP-Endgeraete-Anbindung (eigener Chat, pausiert)
Saison 14:  Lizenz-Server-Fleisch, CA pro Lizenz, TLS-Klient
            Eigener Interface-Designer im unifix-Admin (entzieht
            uns die Abhaengigkeit von UA-WebUI fuer
            Receiver-Verknuepfungen).
Saison 15+: Hardware-Bindung, Plattform-Erweiterungen
            (Stempelkarten-Plugin mit door_events und
             time_clock_entries, Hash-Chain fuer Append-Only)
Saison 16+: Eigene Intercom-Hardware auf ESP32-Basis,
            ggf. Open-Source-Strategie-Review (offen)
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
Plattform-DB referenziert sie nur via `ua_user_id`-Foreign-Key-
Konzept.

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

### 9.3 Tabellen-Inventar (Stand S12-03)

```
schema_version       Migrations-Tracking. PK version, applied_at.
                     Migration 001.

magic_link_tokens    Magic-Link-Auth-Tokens. PK token (Klartext,
                     43-char base64url), ua_user_id, created_at,
                     expires_at, consumed_at (nullable). Migration 001.
                     Index idx_magic_link_ua_user.

sessions             Mieter-Browser-Sessions. PK session_id
                     (Klartext, 43-char base64url), ua_user_id,
                     created_at, last_seen, expires_at, user_agent,
                     ip. Migration 001. Indexes idx_sessions_ua_user
                     und idx_sessions_expires.

mock_viewers         Persistierte Mock-Viewer-Configs. PK mac
                     (z.B. "0c:ea:14:42:42:42"), name, service_port
                     (UNIQUE-Index), ua_user_id (nullable), created_at,
                     updated_at. Migration 002. Indexes
                     idx_mock_viewers_ua_user und
                     idx_mock_viewers_port (UNIQUE).
```

### 9.4 Zukuenftige Tabellen-Roadmap

```
door_events          Audit-Trail fuer Webhook-Empfang in S12-07.
                     Felder: ts, ua_user_id, action ("doorbell",
                     "unlock", "cancel", "reject"), source ("ua",
                     "tenant", "admin"), request_id, raw_payload.
                     Hash-Chain optional fuer Append-Only-Garantie.
                     Migration 003.

time_clock_entries   Stempelkarten-Plugin (S15+). Felder: ts,
                     ua_user_id, kind ("in"/"out"/"break"),
                     location_id, source, hash_prev, hash_self.
                     Append-Only mit Hash-Chain. Migration 004+.

plugin_data          Generic Plugin-Storage fuer S15+ Plugin-System.
                     Felder: plugin_id, ua_user_id, key, value_json,
                     created_at, updated_at. Migration 004+.
```

### 9.5 Foreign-Key-Konzept zu UA-Welt

```
ua_user_id           Stringform der UA-User-ID. KEIN echter SQLite-
                     Foreign-Key zu einer lokalen Tabelle weil UA
                     der Source-of-Truth ist und wir die UA-User-
                     Stammdaten nicht spiegeln wollen. Synchronisation
                     ist Pull-on-Demand via /api/v1/developer/users.
                     Saison-12-Convenience: nullable, leere
                     Verknuepfung ist erlaubt (z.B. Mock-Viewer
                     noch ohne Mieter-Zuordnung).
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
   schreibt direkt auf einen `chan DoorbellEvent`. Der Browser-
   Push-Hub (S12-05) liest aus dem Channel. Kein RPC zwischen
   Prozessen, keine Socket-Datei, keine MessagePack-Serialisierung.

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

### 10.3 Manager-Pattern

`server/internal/mockmanager` ist die Goroutine-Ownership-Schicht.
Verantwortlichkeiten:

```
LoadFromDB(ctx)            Beim Server-Start: alle mock_viewers
                           lesen, je eine Viewer-Goroutine spawnen.

AddViewer(ctx, spec)       INSERT in mock_viewers, dann spawn.
                           Returnt ErrMACInUse / ErrPortInUse bei
                           Kollision mit bereits laufendem Viewer.

RemoveViewer(ctx, mac)     Cancel + Wait + DELETE.

UpdateUserBinding(ctx, mac, uaUserID)
                           Aendert mock_viewers.ua_user_id ohne
                           den Viewer zu stoppen (Verknuepfung ist
                           Plattform-State, UDM weiss nichts davon).

LookupUserByMAC(ctx, mac)  DB-Direct-Query (umgeht den Manager-
                           Mutex), liefert ua_user_id fuer den
                           Klingel-Routing-Hot-Path.

ListViewers(ctx)           Snapshot aller laufenden Viewer fuer
                           das Admin-UI.

Events()                   Liefert den Multiplex-Channel aller
                           DoorbellEvents.

Cancels()                  Analog fuer DoorbellCancelEvents.

Shutdown(ctx)              Cancel't alle Viewer und wartet mit
                           Deadline.
```

### 10.4 Multiplex-Channel-Pattern

Jeder mock.Viewer hat eigene events- und cancels-Channels (16
gepuffert). Der mockmanager startet pro Viewer ZWEI Forwarder-
Goroutines die aus den Per-Viewer-Channels lesen und in zwei
gemultiplexte Manager-Channels schreiben (64 gepuffert). Vorteile:

- Browser-Push-Hub (S12-05) sieht nur EINEN Channel statt N.
- Stage-Goroutine blockiert nie an einem langsamen Consumer,
  weil sie non-blocking in den Per-Viewer-Channel schreibt.
- Forwarder kann auf eigene ctx-cancel reagieren wenn ein
  einzelner Viewer entfernt wird.

Drop-Verhalten bei voller Buffer: warn-Log mit MAC + RequestID.
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
- ESP-Saison-Wiederaufnahme (Saison 13): das ESP-Geraet wird
  vermutlich aehnlich wie der Mock einen einzelnen Stage-Stack
  brauchen.
- Wire-Format-Verifikation gegen pcap-Goldminen.

---

## 11. Authentifikations-Schicht (Saison 12)

### 11.1 Mieter-Auth (Pfad `/m/`)

```
Mechanismus:    Magic-Link via E-Mail (Sascha sendet manuell,
                Automatisierung in S12-04). 32-Byte base64url-
                Token mit 7d-TTL. Konsum tauscht Token gegen
                Session-Cookie.

Cookie:         Name "unifix_m_session", Pfad "/m/", HttpOnly,
                SameSite=Strict, Secure (ausser DevMode).
                MaxAge 30d entsprechend Session-TTL.

Session:        43-char base64url-ID, gespeichert plain in
                sessions-Tabelle. Validate macht Rolling-Renewal
                (last_seen + expires_at = now + 30d) in einer Tx.

Login-Endpoint: GET /m/login?t=<token>.
                Optimistic Session-Check vor Token-Consume:
                eingeloggter Klient wird redirected ohne den
                Token zu verbrauchen.

Logout:         POST /m/logout, revokes Session + clear Cookie.
```

### 11.2 Admin-Auth (Pfad `/a/`, Saison 12-04)

```
Mechanismus:    Username + Passwort (Plan: bcrypt-Hash in einer
                neuen admins-Tabelle, Migration in S12-04).
                Optional 2FA-TOTP fuer eine spaetere Saison.

Cookie:         Name "unifix_a_session", Pfad "/a/". Sonst wie
                Mieter-Cookie.

Session-Tabelle: gleiches sessions-Schema, ggf. ein Discriminator-
                Feld oder eine separate admin_sessions-Tabelle.
                Entscheidung in S12-04.
```

### 11.3 UA-Developer-API-Token

```
Mechanismus:    API-Key im Header X-API-KEY oder Bearer-Token.
                Token wird vom Anlagen-Admin im UniFi Portal
                erzeugt und per env (UNIFIX_UA_API_TOKEN, geplant
                fuer S12-05) an unifix-server uebergeben.

Lebenszyklus:   einmalige Setup-Aktion pro Anlage, kein
                Refresh-Mechanismus von unserer Seite.

Storage:        nur im Process-Memory. Niemals in Logs oder
                Error-Reports, niemals in Saison-Protokollen,
                niemals an Browser-Klienten weitergegeben.
                Siehe docs/security.md Sektion 2.2.4.
```
