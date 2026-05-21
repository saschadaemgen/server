# carvilon Security Plan

**Status:** Saison 14 abgeschlossen 19. Mai 2026 (S14-DOKU).
Lebendes Dokument, wird pro Saison ergaenzt.
**Stand:** Strategische Eckpunkte gesetzt. Saison 12 hat den
Auth-Backbone (Magic-Link plus Mieter- und Admin-Session), die
TLS-Schicht im Server-Prozess, das AES-256-GCM-Verschluesseln
von UA-API-Tokens (platform_config) und den FK-CASCADE-Datenpfad
fuer Mock-zu-Sessions/Tokens umgesetzt. Saison 13 hat den
ESP-Bearer-Token, den Stream-Reverse-Proxy mit Header-Strip und
die Hub-Door-Audit-Vorbereitung dazu gestellt. Saison 14 hat
das Settings-Surface mit Allow-Listen gehaertet, Mieter-Soft-
Delete vom Admin-Audit-Trail entkoppelt, `config.changed`-SSE-
Broadcasts mit per-viewer_mac-Filter eingefuehrt und Admin-
Inline-Edit-Endpoints (Stammdaten, Settings, Passwort, ESP-
Token-Regen) freigeschaltet. Hardware-Bindung und Lizenz-
Server-TLS bleiben Saison 15+.
**Geltungsbereich:** intern, Geschaeftsgeheimnis.

## 1. Sicherheits-Philosophie

carvilon ist eine Convenience-Plattform, kein Sicherheits-Produkt.
Mieter-Authentifizierung lauft auf Convenience-Niveau (Magic-Link),
nicht Bank-Sicherheit. Hochsensitive Bereiche brauchen die nativen
UniFi-Reader und Hub-Door-Mechanismen.

Trotzdem werden alle Komponenten mit Branchen-Standards gehaertet:
TLS wo Verschluesselung sinnvoll ist, Hardware-Bindung gegen
Software-Klau, Source-Code-Schutz gegen Reverse-Engineering.

## 2. Schichten und ihre Sicherheits-Beduerfnisse

### 2.1 UniFi-Seite (Mock <-> UDM)

KOMPLETT VERSCHLUESSELT von Tag eins. UniFi verlangt das:

- HTTPS mit Server-Cert fuer Adoption-Endpoint :8080
- WSS mit JWT-Auth fuer Notification-Channel :12443
- MQTTS mit mTLS fuer RPC und Heartbeat :12812

Cert-Material kommt aus dem Adoption-Bundle (Saison 8 + 9
Reverse-Engineering). Keine Arbeit fuer uns ausser korrektem
TLS-Setup im Go-Code.

### 2.2 Mieter-Klient-Seite (Endgeraet <-> carvilon-server)

```
Saison 10-11:  HTTP plain im LAN, Magic-Link-UUID als Token
               Bewusst Convenience-Niveau, kein Sicherheits-Versprechen.

Saison 12:     IMPLEMENTIERT. TLS-Layer direkt im carvilon-server-
               Prozess (Variante 3b). HttpOnly + SameSite=Strict
               Cookie auf Pfad /m/ (Mieter) und /a/ (Admin, S12-04).
               DevMode-Schalter fuer lokale Entwicklung mit
               Plain-HTTP, niemals in Production.

spaetere      TLS-Reifephasen (Self-Signed mit Fingerprint-Akzept
Saisons:       beim Erstkontakt, dann Kunden-Eigen-CA via Lizenz-
               Server) sind in eine spaetere Saison verschoben.
               Saison 13 ist nach dem S13-DOC-00-Re-Scope mit
               Doorbell-History, UI-Politur und Stream belegt;
               Saison 14 ist Webhook. Der TLS-Cert-Ausbau wird
               zusammen mit dem Lizenz-Server-Fleisch geplant
               sobald die erste Pilot-Anlage konkret ansteht.
```

#### 2.2.4 API-Token-Sicherheit

Saison 12+ verwendet die offizielle UniFi Access Developer API
(siehe wire-format.md und CLAUDE.md Sektion 21). Auth ist
ausschliesslich `Authorization: Bearer <token>` mit einem im
UniFi Portal vom Anlagen-Admin erzeugten Token. Der frueher
mitdokumentierte `X-API-KEY`-Header ist NICHT in der offiziellen
Doku v4.2.16 Sektion 2.7 erwaehnt und wurde im S12-04-Hotfix
verworfen: UA antwortet auf X-API-KEY mit
`CODE_UNAUTHORIZED` selbst wenn der Token an sich gueltig waere.

Das API-Token gibt VOLLEN Zugriff auf User-CRUD, Door-Unlock,
Doorbell-Trigger usw. Es muss daher:

- niemals im Browser oder Endgeraet landen
- niemals in Logs oder Error-Reports erscheinen
- niemals in Saison-Protokollen oder Goldminen-Files persistieren
- nur im carvilon-server-Process-Speicher als entschluesselter
  Wert leben; persistent gespeichert wird ausschliesslich die
  AES-256-GCM-Variante in `platform_config.value_encrypted`
  (Details Sektion 7.4)
- pro Anlage einmalig vom Admin im Admin-UI `/a/settings`
  gesetzt werden, nicht generierbar vom carvilon-server selbst

Der Browser/Endgeraet-Klient redet ausschliesslich mit dem
carvilon-server (eigener Magic-Link), nicht direkt mit der UDM-API.

#### 2.2.5 Magic-Link und Session Klartext-Storage (Saison 12)

Magic-Link-Tokens und Session-IDs werden in Saison 12 weiterhin
PLAIN in der SQLite-DB gespeichert. Beide sind 32 Bytes
crypto/rand, base64url-encoded (43 Zeichen ASCII).

Bewusster Trade-off, Sascha-Beschluss 12. Mai 2026:

```
Risiko-Modell:    Single-Tenant pro Anlage. Die SQLite-Datei
                  liegt im Server-Process unter ./state/carvilon.db
                  mit File-Mode 0600 (Unix) bzw. nur fuer den
                  Service-User lesbar (Windows). Der einzige
                  legitime Reader ist der carvilon-server-Prozess
                  selbst. Lokale Angreifer-Annahme: wer
                  File-Zugriff auf state/ hat, hat auch
                  Process-Memory und damit ohnehin volle
                  Kontrolle.

Konsequenz:       Hashing der Tokens wuerde keinen praktischen
                  Sicherheits-Gewinn bringen. Wir koennen den
                  Storage-Layer in einer spaeteren Sicherheits-
                  Review-Saison auf Hash und Verify migrieren
                  falls Multi-Tenant pro Anlage oder Cloud-
                  Hosting-Modelle relevant werden.

Klartext-Logs:    Tokens und Session-IDs werden NIE im Klartext
                  geloggt. Falls geloggt, dann maximal die
                  ersten 8 Zeichen als Praefix (siehe
                  CLAUDE.md DON'T-Liste).

S12-06-Refactor:  magic_link_tokens und mieter_sessions haengen
                  beide per Foreign-Key mit ON DELETE CASCADE
                  an mock_viewers.mac (alle drei Objekte spaeter
                  durch Migration 006 umbenannt: viewers,
                  viewer_sessions; magic_link_tokens komplett
                  entfernt mit dem Magic-Link-Feature). Loescht
                  der Admin einen Mock-Viewer, verschwinden alle
                  aktiven Mieter-Sessions UND alle ausstehenden
                  Magic-Link-Tokens dieses Mocks automatisch mit.
                  Das ist gewollt: ein Mock-Viewer ist der
                  Routing-Endpunkt, ohne ihn gibt es keinen
                  legitimen Zugang. admin_sessions haengt analog
                  per FK CASCADE an admin_users.
```

#### 2.2.6 Cookie-Sicherheit (Saison 12, Saison 13-02-FIX4-a)

Session-Cookies sind defensiv konfiguriert; die exakte Form
unterscheidet sich zwischen Production und DevMode, weil der
__Host-Prefix nur ueber HTTPS funktioniert.

```
Production (Secure=true):
   Viewer-Cookie: __Host-carvilon_viewer  Path=/
   Admin-Cookie:  __Host-carvilon_admin   Path=/
   Trennung ueber den Cookie-NAMEN, nicht den Pfad - der
   __Host-Prefix verlangt Path=/ und Domain-Pinning per RFC
   6265bis. Beide Cookies liegen daher unter /, der Browser
   kann sie aber nicht verwechseln, weil sie unter
   unterschiedlichen Namen gespeichert werden.

DevMode (Secure=false, plain HTTP):
   Viewer-Cookie: carvilon_viewer        Path=/
   Admin-Cookie:  carvilon_a_session     Path=/a/
   Kein __Host-Prefix moeglich (verlangt Secure). Trennung
   ueber den Namen wie in Production, plus zusaetzliche
   Pfad-Trennung beim Admin (Path=/a/). Akzeptierter Trade-
   off; DevMode laeuft nur lokal auf dem Entwickler-Rechner.

In beiden Modi gilt:
   HttpOnly:   true              (immer, kein JavaScript-Zugriff)
   SameSite:   Strict            (immer, kein Cross-Site-Sending)
   MaxAge:     1 Jahr            (DB-Rolling-Renewal in
                                  session.Validate macht die
                                  effektiven 30 Tage Idle-Loesung)
```

Hintergrund S12-06-Refactor: die Mieter- und Admin-Sessions
leben in getrennten DB-Tabellen (viewer_sessions, admin_sessions;
Migration 006 hat mieter_sessions in viewer_sessions umbenannt).
Das Cookie ist die transportierte ID; die Tabellenwahl entscheidet
ueber die ausgelesene Identitaet. Cookie-Name und Tabelle gehoeren
zusammen - admin_sessions-Validate prueft niemals einen Mieter-
Cookie und umgekehrt.

`SameSite=Strict` ist die maximale Stufe. Wir akzeptieren bewusst,
dass externe Links zu carvilon-Seiten den Klienten nicht
automatisch eingeloggt zeigen (er muss erst ueber /login mit
seinen Mieter-Credentials reinkommen).

#### 2.2.7 DevMode-Schalter

`UNIFIX_DEV_MODE=1` aktiviert lokale Entwicklung:

```
- ListenAndServe statt ListenAndServeTLS (Plain HTTP)
- Cookie-Secure=false (sonst koennten Browser den Cookie nie
  akzeptieren weil das Plain-HTTP-Setup nicht TLS ist)
- ListenAddr-Default :8080 statt :8443
- BaseURL-Default http://localhost:8080

Strikt NUR fuer lokale Entwicklung auf Saschas Windows-Rechner.
Production startet ohne UNIFIX_DEV_MODE und mit CertFile /
KeyFile, sonst lehnt Config.Validate den Start ab.
```

### 2.3 Lizenz-Server-Seite (RPi <-> Cloud)

TLS PFLICHT ab Tag eins der Saison 14. Lizenz-Validierung ohne
TLS ist trivial spoofbar. Konkret:

- Cloud-Server hat Let's-Encrypt-Cert oder Eigen-CA-Root
- RPi-Client prueft Cert-Validitaet und ggf. Pin
- Lizenz-Schluessel sind asymmetrisch signiert, nicht nur
  Bearer-Tokens

## 3. Lizenz- und Hardware-Bindung

Beschluss Saison-10-Abend: jede Lizenz wird an die RPi-Hardware
gebunden. Mehrere Stufen, von einfach zu hart:

**Saison-Zeitfenster:** die in den Sub-Sektionen 3.1 bis 3.4
genannten Saison-Nummern (Saison 14 / 15+ / 16+) stammen aus
dem Roadmap-Stand vor S13-DOC-00. Mit dem Saison-13-Re-Scope
ist der Lizenz-Server-Ausbau zeitlich offen verschoben (siehe
CLAUDE.md Sektion 15 und docs/architecture.md Sektion 7); die
Reifegrad-Reihenfolge der Bindungs-Stufen bleibt erhalten, die
konkrete Saison-Zuordnung wird zusammen mit dem Lizenz-Server-
Briefing neu gesetzt sobald die erste Pilot-Anlage ansteht.

### 3.1 Stufe A: Seriennummer-Binding (Default, Saison 14)

Beim ersten Online-Check merkt der Lizenz-Server die RPi-
Seriennummer aus /proc/cpuinfo. Lizenz ist ab dann an diese
Seriennummer gebunden.

Schutz gegen: SD-Karten-Klau, "einmal kaufen tausend Mal laufen".
Kein Hardware-Eingriff noetig, kein Risiko.

### 3.2 Stufe B: CA-Private-Key-Sealing (Optional, Saison 15+)

Der pro-Lizenz-CA-Private-Key wird verschluesselt mit einem
Geraete-spezifischen Schluessel der aus RPi-OTP/Hardware-
Seriennummer abgeleitet wird. Nur dieser RPi kann den Key
entschluesseln und nutzen.

Schutz gegen: SD-Karte klauen + CA-Key auslesen + eigene
Klingel-Hardware bauen.

Erfordert: Detail-Forschung zur RPi-OTP-API und Boot-Sicherheit.

### 3.3 Stufe C: eFuse / OTP-Brennen (Risiko-behaftet, evtl. Saison 16+)

Wenn wirklich Hochsicherheits-Kunden bedient werden, koennen
Customer-Programmable-OTP-Bits gebrannt werden (32-bit-Worte, 8
Stueck verfuegbar auf BCM2711/2712). Einmal-Brennen, nie zurueck.

Use-Cases:

- Erstes-Boot-Datum festschreiben (Anti-Wiederverkauf)
- Pro-Geraet-Identifier (jenseits der gratis Seriennummer)
- Customer-Boot-Secret fuer Secure-Boot-Chain

HARTE CAVEATS:

- Falsch gebrannter Bit = RPi muss weggeworfen werden
- Pro-Kunde-Test-Stufe muss bombensicher sein
- Erfordert dediziertes Saison-Investment, nicht im Plan

### 3.4 Stufe D: TPM oder YubiHSM (Industrie-Niveau, undefiniert)

Fuer Bank/Militaer-Niveau braucht es dedizierte Hardware
(Industrie-RPi mit TPM-Onboard oder externes Secure Element).
Konzept-Architektur muesste fuer einzelne Premium-Lizenzen
ueberdacht werden.

## 4. Source-Code-Schutz

### 4.1 Build-seitig (ab Saison 10 implementiert)

```
- ldflags="-s -w"    Symbol-Table und Debug-Info weg
- trimpath           Source-Pfade weg
- CGO_ENABLED=0      pure Go, kein libc-Tracing-Vektor
- single Binary      keine separaten Konfig- oder Lib-Dateien
```

### 4.2 Source-Distribution

- KEIN Push zu GitHub oder anderen Remote-Hostern (deny-Regel
  in .claude/settings.local.json)
- KEIN Open-Source-Hinweis im Code, README, Marketing
- Saison-Protokolle und CLAUDE.md sind interne Dokumente,
  niemals oeffentlich

### 4.3 Optional in spaeterer Saison: garble (Go-Obfuskator)

Macht Reverse-Engineering deutlich schwerer durch:

- Identifier-Mangling
- Konstanten-Verschleierung
- Control-Flow-Obfuskation

Trade-off: erschwert auch eigenes Debugging. Erst sinnvoll wenn
Produkt produktiv und stabil ist.

## 5. Bedrohungsmodelle die wir adressieren

| Bedrohung                        | Schutz                  | Saison |
|----------------------------------|-------------------------|--------|
| Anderer Mieter sniffed im WLAN   | LAN-only-Traffic, TLS   | 12+    |
| WLAN-Passwort kompromittiert     | Magic-Link + TLS-Cert   | 12+    |
| SD-Karte geklaut, RPi geklont    | Hardware-Bindung A      | 14     |
| SD-Karte geklaut, CA extrahiert  | Key-Sealing B           | 15+    |
| Source-Code geleakt              | Build-Strip + Obfuskat  | 10/14  |
| Lizenz-Server-Spoofing           | TLS + signierte Lizenz  | 14     |
| Fake-Lizenzschluessel            | Asymmetrische Signatur  | 14     |

## 6. Bedrohungsmodelle die wir NICHT adressieren

- Physischer Zugriff auf den RPi (offene Tuer-Steckdose...)
- Boeswilliger Admin mit Root auf dem RPi
- Boeswilliger Klingel-Knopf-Druecker (per Design ueber UniFi-Reader)
- Bot-Netze gegen den Lizenz-Server (in Saison 14 ggf. Rate-Limits)
- Quantum-Crypto-Bedrohungen (out of scope, Klassik-TLS reicht)

---

## 7. Plattform-Daten-Sicherheit (Saison 12)

### 7.1 SQLite-Datei-Schutz

```
Pfad:           ./state/carvilon.db (default, ueberschreibbar
                via UNIFIX_DB_PATH)
File-Mode:      0600 (db.Open via os.MkdirAll setzt Parent-Dir
                auf 0700, SQLite legt die Datei mit 0644 an die
                wir per umask oder File-Mode-Set nach 0600
                bringen koennten. Windows ignoriert POSIX-Modes.)
Concurrency:    SetMaxOpenConns(1) sichert dass nur eine
                Connection aktiv ist. WAL-Mode erlaubt
                trotzdem schnelle parallele Lese-Queries.
Backup-Strategie: in einer spaeteren Saison zusammen mit dem
                Lizenz-Server-Ausbau zu klaeren. Vorlaeufig:
                simple File-Copy bei Service-Stop, der
                Lizenz-Server kann spaeter eine differential-
                Backup-API bereitstellen.
```

### 7.2 Migration-Sicherheit

```
Atomic:         Jede Migration laeuft in einer eigenen
                BEGIN/COMMIT-Transaktion. Bei Fehler ROLLBACK,
                db.Open gibt Fehler zurueck, Server startet
                NICHT (das ist Absicht: laufende Server-Instanzen
                mit halb-applizierten Schemas waeren ein
                Daten-Konsistenz-Risiko).
Idempotenz:     schema_version-Tabelle verhindert doppelte
                Anwendung. Reboot eines bereits migrierten
                Servers tut nichts neu.
Embed:          Migrations-Files sind in das Binary einkompiliert
                via go:embed. Es gibt keine externe
                Migration-Source die manipulierbar waere.
```

### 7.3 Mock-State-Verzeichnisse

```
Pfad:           ./state/mocks/<mac>/   pro Mock-Viewer ein
                eigenes Unterverzeichnis. Default-Parent ist
                UNIFIX_MOCK_STATE_DIR.
File-Mode:      0700 fuer Verzeichnisse, 0600 fuer einzelne Files
                (bundle.json, meta.json, jwt.json, certs/*.crt,
                certs/*.key, last_doorbell.json,
                runtime_config.json).
Atomic-Writes:  state-Paket nutzt temp-file + rename damit
                ein Crash mid-write keine halben Files
                hinterlaesst (siehe state.writeFileAtomic).
Sensitive:      bundle.json und certs/broker.key enthalten
                mTLS-Material des Mock-Viewers fuer den UDM-
                MQTT-Broker. Bei Klau aus dem state-Verzeichnis
                kann ein Angreifer auf den UDM-MQTT-Broker
                connecten und als der gekuemerte Mock auftreten.
                Mitigation: Service-User-Isolation auf dem RPi
                (siehe Saison 15+ Hardening).
```

### 7.4 Plattform-Secrets-Verschluesselung (Saison 12-04)

Das `secrets`-Paket implementiert AES-256-GCM-Verschluesselung
fuer einzelne Werte in der `platform_config`-Tabelle. Hauptkunde
ist aktuell der UA-API-Token; weitere sensitive Settings koennen
auf demselben Pfad lagern.

```
Algorithmus:    AES-256-GCM (Authenticated Encryption with
                Associated Data). 256-Bit-Key, 96-Bit-Nonce,
                128-Bit-Tag. crypto/aes + crypto/cipher aus
                der Go-Stdlib, kein externes Krypto-Modul.

Master-Key:     Aus env-Variable UNIFIX_SECRETS_KEY (64 hex
                chars, 32 Bytes raw). Wird im main.go beim
                Server-Start gelesen; fehlt der Key, refused
                der Server den Start.
                Erzeugung: cmd/genkey-Tool liest 32 Bytes von
                crypto/rand und gibt den hex-Encoded String
                aus. Operator-Konvention: einmal generieren,
                pro Anlage konstant halten.

Nonce:          12 Bytes crypto/rand pro Wert. Wird als
                Praefix vor den Ciphertext serialisiert, sodass
                Decrypt selbst-tragend ist:
                  hex(nonce || ciphertext || tag)
                in platform_config.value_encrypted.

API:            secrets.Service.Encrypt(plaintext) []byte
                secrets.Service.Decrypt(ciphertext) []byte
                platformconfig.SetSecret(ctx, key, value) und
                .GetSecret(ctx, key) wrappen das fuer den
                Datenbank-Pfad.

Storage:        platform_config-Zeile setzt entweder value
                (Klartext) oder value_encrypted (hex), nie
                beides. Wer in beide schreibt: ist ein Bug
                im Caller, kein DB-Constraint (eine spaetere
                Saison koennte das verhaerten falls noetig).

Key-Rotation:   Wechsel des UNIFIX_SECRETS_KEY macht alle
                bestehenden value_encrypted-Werte ungueltig
                (AES-GCM-Auth-Tag schlaegt fehl). Konsequenz:
                Admin muss nach Key-Wechsel den UA-Token im
                Admin-UI /a/settings neu eintragen. Es gibt
                keinen Re-Encrypt-Pfad im Server (bewusst, weil
                das Master-Key-Verlust-Szenario praktisch nicht
                wiederherstellbar sein soll).

Operator-      Master-Key ist Teil der Operator-Verantwortung,
Verantwortung: NICHT im SQLite-File enthalten. Verliert der
                Operator den Key, gehen alle verschluesselten
                Werte verloren (Re-Setup noetig). Empfohlene
                Aufbewahrung: 1Password / KeePass / sealed
                envelope, nicht im selben Backup wie die DB.

Klartext-Logs:  Master-Key NIEMALS loggen, auch nicht im
                Debug-Mode. Verschluesselte Werte koennen
                geloggt werden (sind ja gerade verschluesselt).
                Bei der Operation auf dem Plaintext (z.B.
                Outgoing UA-API-Call) gilt die 8-Zeichen-Praefix-
                Regel wie fuer alle Tokens.
```

### 7.5 ESP-Bearer-Token (Saison 13-08)

Im Gegensatz zur Mieter-Auth (Magic-Link + Cookie-Session) und
zur Admin-Auth (Username + bcrypt + Cookie-Session) laueft die
ESP-Endgeraete-Authentifikation ueber einen geraete-skopten
Bearer-Token, der pro adoptiertem ESP einmalig generiert und im
NVS des Geraets persistiert wird.

```
Generierung:    32 Bytes von crypto/rand. base64url-encoded
                ergibt 43 ASCII-Zeichen ohne Padding,
                ~256 Bit Entropie. Code:
                  server/internal/auth/esptoken.Generate
                Maschine-zu-Maschine-Auth, kein User-Eingabe-
                Material - SHA-256 als Storage-Hash reicht
                (Argon2id waere Overkill, der Token ist nicht
                offline brute-force-anfaellig).

Speicherung:    NUR der SHA-256-Hash in
                viewers.esp_token_hash (hex-encoded). Klartext
                wird NIEMALS persistent gespeichert.
                Beim Adoption-Flow:
                  - Workflow Discover-First: Klartext wird
                    voruebergehend in
                    esp_pending_devices.adopted_token_cleartext
                    geparkt, beim ersten /esp/discover/status
                    Long-Poll an das ESP ausgeliefert und die
                    pending-Zeile sofort GELOESCHT (single-
                    delivery).
                  - Workflow CLI-First: Klartext wird einmalig
                    auf stdout des carvilon-cli ausgegeben; gar
                    nicht erst in einer DB-Spalte gespeichert.
                In beiden Faellen ist der Klartext nach der
                ersten Auslieferung WEG. Verlorene Tokens =
                Token-Regenerate-Workflow.

Wire-Format:    Authorization: Bearer <token>
                Server-seitige Verifikation in
                  server/internal/auth/esptoken.Verify
                via crypto/subtle.ConstantTimeCompare gegen
                Timing-Leaks.

Lookup:         Linear-Scan ueber alle
                viewers WHERE type='esp' AND esp_token_hash
                IS NOT NULL. Bei <100 ESPs pro Server (realistisch
                fuer eine Wohnanlage) billig genug; spaetere
                Saison kann auf indizierten Hash-Lookup umstellen
                wenn Multi-Tenant-Server gewachsen sind.

Revocation:     "Token erneuern" im /a/esp-viewers-Modal oder
                CLI-Re-Adopt schreibt einen neuen Hash ueber den
                alten - der alte Token ist ab dann inaktiv (Hash-
                Lookup matcht nicht mehr). Es gibt keine separate
                Revocations-Liste; der ueberschriebene Hash ist
                der Revocation.
                Komplettes Loeschen des ESP-Eintrags (DELETE-
                Button im Admin-Modal) entfernt die Zeile aus
                viewers; die Sibling-FK-CASCADE-Loescht
                assoziierte ungelesene door_events nicht (die
                bleiben mit der MAC referenziell erhalten -
                gewollt, sonst geht der Audit-Trail verloren).

Klartext-Logs:  Bearer-Tokens NIEMALS im Klartext loggen, auch
                nicht im Debug-Mode. Hashes koennen geloggt
                werden (sie sind ja gerade Hashes); fuer
                Klartext gilt die 8-Zeichen-Praefix-Regel wie
                fuer alle Tokens.
```

### 7.6 Stream-Reverse-Proxy (Saison 13-08)

`/esp/stream.mjpeg` ist ein dummer Reverse-Proxy auf
`UNIFIX_STREAM_BACKEND_URL` (typisch ein lokaler go2rtc-Daemon).
Sicherheits-relevante Eigenschaften:

```
Bearer-Filter:  Der Endpoint sitzt hinter requireESPBearer.
                Nur adoptierte ESPs koennen ihn aufrufen.

Token-Stripping: Vor dem Forward an das Backend wird der
                Authorization-Header GESTRIPPT. Das ESP-Token
                darf den carvilon-Process-Boundary nicht
                ueberschreiten - das Backend ist typisch ein
                unauthenticated Localhost-Daemon, der den
                Token weder kennt noch braucht. Wuerde der
                Token weitergeleitet, koennte er in den
                Backend-Logs landen oder bei einem Backend-
                Kompromiss extrahiert werden.

Konfiguration:  UNIFIX_STREAM_BACKEND_URL als Env-Variable
                (server/internal/config: StreamBackendURL).
                Wenn nicht gesetzt: HTTP 503 "stream backend
                not configured". Bewusst ein 503 + nicht ein
                404, damit der ESP-Klient zwischen "Endpoint
                existiert nicht" und "Endpoint existiert,
                Backend gerade aus" unterscheiden kann.

Backend-       Bei Backend-Unreachable (Connection refused,
Unreachable:    Timeout, etc.): HTTP 502 "stream backend
                unreachable" plus Warn-Log mit der konkreten
                Backend-URL. Kein Token in der Logmessage
                (gibts ja nicht mehr - wurde gestrippt).

Spaeter:        S14 Live-View bringt das echte go2rtc-WebRTC-
                Backend; der Reverse-Proxy bleibt das
                primaere Verbindungs-Pfad zur ESP-Hardware.
```

---

## 8. Audit-Trail-Vorbereitung (Doorbell-History S13-01 + Webhook S14)

Die `door_events`-Tabelle (Migration 005) wird in Saison 13-01
angelegt. Der `doorbellhub` schreibt eingehende Klingel-Events
parallel zur SSE-Distribution dorthin, das Mieter-UI rendert die
letzten N Eintraege plus einen Ungelesen-Indikator. Saison 14
dockt zusaetzlich den UA-Webhook-Endpoint
`POST /webhook/access` an dieselbe Tabelle an (Event-Type-
Dispatch), sodass auch UA-Direktevents (z.B.
access.door.unlock ohne vorheriges Klingeln) persistiert werden.

Geplante Pflicht-Felder:

```
ts            Unix-Millisekunden des Events
mock_mac      Welcher Mock-Viewer war am Routing-Pfad beteiligt
              (NULL bei reinen UDM-Events ohne Mock-Bezug)
action        "doorbell" / "unlock" / "cancel" / "reject"
source        "ua" (von UA-Webhook) / "tenant" (Browser) /
              "admin" (Admin-UI) / "mock" (interner Test)
request_id    Korrelations-ID, ggf. die MQTT-requestId oder
              UA-Webhook-request_id
raw_payload   Original-JSON aus dem Webhook, fuer
              forensische Analyse
```

Hash-Chain-Pattern (Saison 16+ Stempelkarten-Plugin):

```
hash_prev     SHA-256 des vorherigen Eintrags
hash_self     SHA-256 dieses Eintrags inkl. hash_prev

Append-Only-Garantie: jede nachtraegliche Aenderung an einem
alten Eintrag bricht die Chain ab dem geaenderten Punkt. Pruefer
kann durch lineares Verfolgen verifizieren ob die Chain intakt
ist. Aenderungen werden so im Audit sofort sichtbar.
```

In Saison 13-01 und 14 zunaechst OHNE Hash-Chain. Eine
spaetere Migration kann die Chain nachruesten, sobald die
Stempelkarten-Anforderung (Saison 16) fest steht.

### 8.1 Webhook-Authentifikation (Sicherheits-Aspekt fuer Saison 14)

UA-Webhooks unterstuetzen Signed-Body via HMAC-SHA256 mit einem
Shared-Secret. In Saison 14 muss carvilon-server:

```
- Pro Webhook-Registration ein eigenes Secret pflegen (gespeichert
  in einer noch anzulegenden webhooks-Tabelle, evtl. via
  platform_config.value_encrypted-Pfad).
- Eingehende Bodies gegen den HMAC-Header verifizieren BEVOR
  irgendetwas geparsed wird.
- Bei Mismatch: 401 Unauthorized, kein Persist, Audit-Log.
- Replay-Schutz: nonce oder timestamp-Fenster (typisch 5 min).
```

Der Webhook-Auth-Pfad ist getrennt vom Cookie-basierten Mieter-
und Admin-Auth: HMAC-Signature im Header statt Session-Cookie,
weil das UDM-Backend keine Cookies setzt. Mieter- und Admin-Pfade
bleiben unveraendert.

Konkret-Spezifikation kommt im Saison-14-Briefing.

## 9. Stream-Backend (Saison 14-01)

carvilon terminiert die oeffentlich erreichbaren Stream-Endpoints
(`/esp/stream.mjpeg`, `/webviewer/stream.mjpeg`) selbst und proxyt
nach `UNIFIX_STREAM_BACKEND_URL` (typisch `http://127.0.0.1:1984`).
go2rtc lauscht ausschliesslich auf dem Loopback-Interface; LAN-
Kunden erreichen den Stream nur ueber carvilon-server. Damit haengen
Authentifikation und Rate-Limit am vorhandenen Cookie- und Bearer-
Pfad und nicht an einem zweiten, separat zu haertenden Daemon.

### 9.1 Authorization-Header wird gestrippt

Der Reverse-Proxy entfernt den eingehenden `Authorization`-Header
bevor er den GET an go2rtc absetzt. Konkrete Folge:

- Der ESP-Bearer verlaesst den carvilon-Prozess nicht.
- Mieter-Session-Cookies werden gar nicht erst zu go2rtc
  weitergereicht (anderer Domain-Scope, Browser sendet sie
  ohnehin nicht im img-Request).
- Wenn go2rtc in einer spaeteren Saison einen eigenen Bearer-Mode
  bekommt, fuegt carvilon den dann gezielt im Outgoing-Request hinzu;
  der Klartext-Token aus dem Endgeraet wird NIE direkt
  durchgereicht.

### 9.2 go2rtc-Admin nur via Session

Die /a/streams-CRUD-Endpoints laufen hinter `requireAdminSession`.
go2rtc selbst kennt keine Authentifikation - es ist nur sicher
solange es auf 127.0.0.1 gebunden ist. Operator-Pflicht:

- `api.listen` in go2rtc.yaml MUSS `127.0.0.1:1984` sein (siehe
  go2rtc.yaml.example im Repo-Root).
- Iptables / firewall darf 1984 nicht extern oeffnen.
- carvilon-server bleibt einzige Frontline-Komponente.

### 9.3 Profil-Source-URLs sind Klartext

Stream-Profile-Source-URLs (RTSPS-URLs mit eingebettetem Token aus
UniFi Protect) werden in der go2rtc.yaml KLARTEXT gespeichert.
Konsequenz:

- Datei-Mode 0600, owned by der go2rtc-User (Default-Setup auf RPi).
- KEIN Backup der yaml in unverschluesselten Kanaelen (kein git,
  kein scp ohne SFTP).
- Bei Stream-Token-Rotation in UniFi Protect MUSS der Operator das
  Profil in /a/streams aktualisieren - die alte URL bleibt sonst
  funktionslos und der Stream faellt aus.

Eine spaetere Saison kann go2rtc hinter Tailscale oder einer
carvilon-eigenen AES-Wrapper-Schicht legen; fuer S14-01 reicht das
Loopback-Binding.

---

## 10. Saison-14-Settings-Sicherheit

### 10.1 Allow-Lists fuer alle Settings-Felder

Saison 14 hat die Mieter- und ESP-Settings stark erweitert.
Alle Felder werden serverseitig gegen Allow-Lists geprueft.
Klient-seitige Validierung ist Convenience, nicht Sicherheit;
ein Curl-Klient kommt am Browser-UI vorbei und muss am Server
geblockt werden.

| Feld | Konstante | Erlaubte Werte | Storage |
| --- | --- | --- | --- |
| `idle_view_mode` | `IdleViewModeAllowed` (implizit, switch im Setter) | `"screensaver"`, `"livestream"`, `"screen_off"` | TEXT NULL, Resolver-Default `screensaver` |
| `auto_screensaver_seconds` | `mockmanager.AutoScreensaverSecondsAllowed` | `{0, 30, 60, 300, 600}` | INTEGER NULL, 0 = off |
| `screen_off_after_sec` | `mockmanager.ScreenOffAfterSecAllowed` | `{0, 30, 60, 300, 600, 1800}` | INTEGER NULL, 0 = off, ESP-only |
| `brightness_idle` | Range-Check `0..100` | jede Integer | INTEGER NULL, Resolver-Default 70, ESP-only |
| `language` | `mockmanager.LanguageAllowed` | `"de"`, `"en"` | TEXT NULL, Resolver-Default `de`, ESP-only |
| `clock_layout` | `mockmanager.ClockLayoutAllowed` | `"vertical"`, `"horizontal"` | TEXT NULL, Resolver-Default `vertical` |
| `history_capture` | bool (true/false oder "1"/"0") | nur die zwei Werte | INTEGER NULL, Resolver-Default true |
| `name` (Stammdaten) | Trim + Length-Check | 1..64 Zeichen, nicht leer | TEXT NOT NULL |
| `paired_intercom_mac` | `macFormat`-Regex + lowercase | leer oder `xx:xx:xx:xx:xx:xx` | TEXT NULL |
| `stream_profile` | Free-Form (von Admin gesetzt) | jeder String, Trim | TEXT NULL |
| `linked_ua_user_id` | UA-User-Existenz NICHT validiert | jeder String, Trim | TEXT NULL |

Der `linked_ua_user_id`-Eintrag ist bewusst nicht gegen die
UA-API gegengeprueft - das wuerde einen synchronen UA-Call pro
Stammdaten-Save bedeuten. Saison 15+ kann ein Async-Validate-
Pattern dazu legen, wenn falsche User-IDs in der Praxis Probleme
machen.

### 10.2 ESP-only-Felder

`screen_off_after_sec`, `brightness_idle` und `language` sind
ESP-Hardware-Konzepte. Die Server-Validierung lehnt sie auf
`type='web'`-Viewer mit 400 ab (siehe
`handleAdminViewerSettings` und `handleESPSettings`). Damit
kann ein Curl-Klient nicht ueber das Mieter-Surface
ESP-Settings auf einem Web-Viewer setzen die fuer die Mieter-
UI bedeutungslos waeren und das `/esp/config`-JSON
verwirren wuerden.

### 10.3 Skip-Echo im Web-Viewer-Tab

Saison 14-04-Phase2-FIX03 hat einen Echo-Race im
config.changed-Broadcast aufgedeckt:

```
Mieter klickt Settings-Radio
   -> POST /webviewer/settings
   -> Server speichert + broadcastet config.changed
   -> Web-Viewer-Tab faengt sein eigenes config.changed
   -> location.reload() reisst User aus Settings-Mode
```

Fix in `home.html`:

```javascript
window.carvilonIdle.lastOwnSaveAt = Date.now();   // vor POST
// ...
es.addEventListener('config.changed', function () {
  if (Date.now() - lastOwnSaveAt < 1000) return;  // Skip-Echo
  location.reload();
});
```

Cross-Device-Sync bleibt intakt: ein zweiter Browser-Tab oder
ein Admin-Edit kommt typisch >1000ms nach dem letzten Eigen-
Save und faellt deshalb durch den Skip-Filter durch. Skip-
Pattern gehoert in CLAUDE.md unter Lessons.

### 10.4 Pagination-DoS-Schutz

Die `parseHistoryListOpts` in `handler_mieter_history.go`
validiert die Query-Parameter strict:

```
offset  0..10000   Sanity-Bound (mieterHistoryMaxOffset)
limit   1..50      doorhistory.ListOptsMaxLimit ist die Obergrenze
from    YYYY-MM-DD strikt; ungueltig -> 400
to      YYYY-MM-DD end-of-day-inklusiv via endOfDayUnix
        plus From > To -> 400
```

Damit kann ein Klient nicht via `?limit=99999999` versuchen
die Tabelle durch eine grosse Query zu blockieren oder via
`?offset=10000000` der DB-Engine ein lineares Skip-Pattern
aufzwingen. Bei AdminListAll gilt dasselbe Limit (50);
Default ist 50 vs 20 beim Mieter.

### 10.5 Soft-Delete entkoppelt vom Audit-Trail

Mieter koennen einzelne Eintraege oder den kompletten Verlauf
soft-loeschen (S14-04-Phase 2). Das Datenmodell:

```
door_events (Migration 005)          unveraendert, alle Rows
                                      bleiben fuer immer (bis
                                      Speicherdauer-Cleanup
                                      kommt, siehe Halde)
viewer_hidden_events (Migration 016) Mieter-Marker
                                      (viewer_mac, event_id,
                                       hidden_at)
                                      FK CASCADE auf viewers.mac
                                      FK CASCADE auf door_events.id
```

`ListVisible` macht LEFT JOIN gegen `viewer_hidden_events` und
filtert raus wo `vhe.event_id IS NOT NULL`. `AdminListAll`
macht denselben Join, filtert NICHT raus, sondern setzt nur
das `HiddenByViewer`-Flag.

```
Konsequenz:
- Mieter-API zeigt seine Soft-Delete-Ansicht
- Admin-API zeigt den vollstaendigen Audit-Trail mit
  Eye-Off-Icon fuer hidden Rows
- Hard-Delete (AdminDeleteEvent) entfernt door_events.id;
  FK CASCADE traegt viewer_hidden_events automatisch mit
- Mieter kann den Audit-Trail NICHT verstecken
```

Das ist die zentrale Sicherheits-Aussage: ein boswilliger
Mieter (oder ein Mieter unter Zwang) kann seine Klingel-
Historie nicht vor dem Hausverwalter verbergen.

### 10.6 history_capture-Toggle und Audit-Trail

`history_capture_enabled = false` (S14-04-Phase 2) blendet die
Mieter-API leer (mit `capture_enabled: false`-Flag im JSON-
Envelope) und blockiert die UnreadCount-Anzeige. Server-seitig
fliessen die door_events trotzdem weiter; die Toggle aendert
nur was die Mieter-UI rendert.

Datenschutz-Wirkung: aus Mieter-Sicht "es wird nicht
mitgeschnitten". Aus Anlagen-Compliance-Sicht: weiterhin
voller Audit. Diese Dual-Lese ist absichtlich (Sasch-Beschluss
S14-04-Phase 2): das Datenschutz-Signal ist UI-only, der
Hausverwalter behaelt den lueckenlosen Trail. Wuerden wir
die Inserts wirklich blocken, koennten Mieter den Trail
aushebeln und die Anlage haftet.

### 10.7 Admin-Inline-Edit-Endpoints

Saison 14-04-Phase 2-FIX02 hat vier neue Admin-Endpoints
freigeschaltet:

```
POST /a/viewers/{mac}/stammdaten         JSON-Body Partial
POST /a/viewers/{mac}/settings           JSON-Body Partial
POST /a/viewers/{mac}/password           Web-only, min 8 Zeichen,
                                          Argon2id-Hash via
                                          storePasswordForViewer
                                          plus sessions.RevokeAllForViewer
POST /a/viewers/{mac}/regenerate-token   ESP-only, esptoken.Generate
                                          plus SetESPTokenHash plus
                                          One-Shot-Klartext-Reveal
                                          im Response
```

Alle vier sitzen hinter `requireAdminSession`. Type-Scoping
(Password nur Web, Token-Regen nur ESP) lebt im Handler.
Die Settings + Stammdaten triggern doorbellhub.
BroadcastConfigChanged damit Cross-Device-Sync greift.

ESP-Token-Regenerate-Spec: der frische Klartext-Token wird
EINMAL im JSON-Response geliefert (`{"ok": true,
"new_token": "...", "mac": "..."}`) und parallel im
`esp_pending_devices.adopted_token_cleartext`-Handoff-Slot
geparkt damit ein laufender ESP-Status-Poll den Token
automatisch uebernimmt. Ein zweiter GET liefert den Token NIE
wieder - das Modal hat einen Copy-Button und einen "Verstanden"-
Button, danach ist der Klartext aus Admin-Sicht weg. Der alte
Bearer-Token ist sofort ungueltig (Hash wurde ueberschrieben).

### 10.8 config.changed-Broadcast pro viewer_mac

`doorbellhub.BroadcastConfigChanged(viewerMAC)` fanout an:

1. Alle Web-SSE-Subscriber auf diesem viewer_mac
   (`/webviewer/events`).
2. Alle ESP-Eventbus-Subscriber auf diesem viewer_mac
   (`/esp/events`).

Filter: pro viewer_mac, kein Cross-Tenant-Leak. Ein Settings-
Save auf Viewer A reicht NIE an Subscriber von Viewer B durch.
Tests `TestBroadcastConfigChanged_FilteredByViewerMAC` und
`TestAdminViewerSettings_TriggersConfigChanged` bewachen das.

Payload ist absichtlich leer (`{}`); Receiver refetchen ihre
Config aus dem zustaendigen GET-Endpoint statt Felder auf dem
Event selbst zu lesen. Verhindert Drift wenn das Event-Schema
sich aendert.

---

## 11. Saison-14-Updates am Bedrohungsmodell

| Bedrohung | Schutz | Saison |
| --- | --- | --- |
| Mieter setzt ungueltigen idle_view_mode | Allow-List, Server-400 | 14 |
| Web-Klient versucht ESP-Settings auf Web-Viewer | Type-Check, Server-400 | 14 |
| Klient pumpt /webviewer/history.json mit limit=99999 | Pagination-Clamp 50 | 14 |
| Klient skip't 99 Millionen Rows mit offset=... | Sanity-Bound 10000 | 14 |
| Mieter versucht Audit-Trail zu verstecken | Admin-AdminListAll sieht alle Rows, FK CASCADE schuetzt Konsistenz | 14 |
| Mieter aendert paired_intercom_mac eines anderen Mieters | requireSession matched auf viewer_mac aus Cookie, Pfad-Param ist nicht beeinflussbar | 14 |
| Admin-Token-Regen lecked alter Token an ESP zurueck | Hash-Ueberschreibung im SetESPTokenHash macht den alten Token sofort ungueltig | 14 |
| Eigener config.changed-Echo loest Reload im Tab aus | Skip-Echo-Heuristik im Web-Viewer (1000ms) | 14 |

---

Zuletzt aktualisiert: 2026-05-19 (Saison-14-Abschluss-Doku).
