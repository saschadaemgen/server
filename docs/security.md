# unifix Security Plan

**Status:** Saison 12 abgeschlossen 12. Mai 2026 (S12-DOC-02).
Lebendes Dokument, wird pro Saison ergaenzt.
**Stand:** Strategische Eckpunkte gesetzt. Saison 12 hat den
Auth-Backbone (Magic-Link plus Mieter- und Admin-Session), die
TLS-Schicht im Server-Prozess, das AES-256-GCM-Verschluesseln
von UA-API-Tokens (platform_config) und den FK-CASCADE-Datenpfad
fuer Mock-zu-Sessions/Tokens umgesetzt; Hardware-Bindung und
Lizenz-Server-TLS bleiben Saison 14+.
**Geltungsbereich:** intern, Geschaeftsgeheimnis.

## 1. Sicherheits-Philosophie

unifix ist eine Convenience-Plattform, kein Sicherheits-Produkt.
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

### 2.2 Mieter-Klient-Seite (Endgeraet <-> unifix-server)

```
Saison 10-11:  HTTP plain im LAN, Magic-Link-UUID als Token
               Bewusst Convenience-Niveau, kein Sicherheits-Versprechen.

Saison 12:     IMPLEMENTIERT. TLS-Layer direkt im unifix-server-
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
- nur im unifix-server-Process-Speicher als entschluesselter
  Wert leben; persistent gespeichert wird ausschliesslich die
  AES-256-GCM-Variante in `platform_config.value_encrypted`
  (Details Sektion 7.4)
- pro Anlage einmalig vom Admin im Admin-UI `/a/settings`
  gesetzt werden, nicht generierbar vom unifix-server selbst

Der Browser/Endgeraet-Klient redet ausschliesslich mit dem
unifix-server (eigener Magic-Link), nicht direkt mit der UDM-API.

#### 2.2.5 Magic-Link und Session Klartext-Storage (Saison 12)

Magic-Link-Tokens und Session-IDs werden in Saison 12 weiterhin
PLAIN in der SQLite-DB gespeichert. Beide sind 32 Bytes
crypto/rand, base64url-encoded (43 Zeichen ASCII).

Bewusster Trade-off, Sascha-Beschluss 12. Mai 2026:

```
Risiko-Modell:    Single-Tenant pro Anlage. Die SQLite-Datei
                  liegt im Server-Process unter ./state/unifix.db
                  mit File-Mode 0600 (Unix) bzw. nur fuer den
                  Service-User lesbar (Windows). Der einzige
                  legitime Reader ist der unifix-server-Prozess
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
                  an mock_viewers.mac. Loescht der Admin einen
                  Mock-Viewer, verschwinden alle aktiven
                  Mieter-Sessions UND alle ausstehenden Magic-
                  Link-Tokens dieses Mocks automatisch mit.
                  Das ist gewollt: ein Mock-Viewer ist der
                  Routing-Endpunkt, ohne ihn gibt es keinen
                  legitimen Zugang. admin_sessions haengt analog
                  per FK CASCADE an admin_users.
```

#### 2.2.6 Cookie-Sicherheit (Saison 12)

Session-Cookies sind defensiv konfiguriert:

```
Name:       unifix_m_session  (Mieter)
            unifix_a_session  (Admin)
Pfad:       /m/  bzw.  /a/    (Pfad-Scoping verhindert dass das
                              Admin-Cookie unter /m/ gesendet wird
                              und umgekehrt; strikt seit dem
                              S12-06-Refactor mit getrennten
                              mieter_sessions- und admin_sessions-
                              Tabellen)
HttpOnly:   true              (immer, kein JavaScript-Zugriff)
Secure:     true in Production, false in DevMode
SameSite:   Strict            (immer, kein Cross-Site-Sending)
MaxAge:     30 Tage           (passend zu Session-Rolling-TTL)
```

`SameSite=Strict` ist die maximale Stufe. Wir akzeptieren bewusst,
dass externe Links zu unifix-Seiten den Klienten nicht
automatisch eingeloggt zeigen (er muss erst ueber /m/login mit
Magic-Link reinkommen).

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
Pfad:           ./state/unifix.db (default, ueberschreibbar
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
Shared-Secret. In Saison 14 muss unifix-server:

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

unifix terminiert die oeffentlich erreichbaren Stream-Endpoints
(`/esp/stream.mjpeg`, `/einloggen/stream.mjpeg`) selbst und proxyt
nach `UNIFIX_STREAM_BACKEND_URL` (typisch `http://127.0.0.1:1984`).
go2rtc lauscht ausschliesslich auf dem Loopback-Interface; LAN-
Kunden erreichen den Stream nur ueber unifix-server. Damit haengen
Authentifikation und Rate-Limit am vorhandenen Cookie- und Bearer-
Pfad und nicht an einem zweiten, separat zu haertenden Daemon.

### 9.1 Authorization-Header wird gestrippt

Der Reverse-Proxy entfernt den eingehenden `Authorization`-Header
bevor er den GET an go2rtc absetzt. Konkrete Folge:

- Der ESP-Bearer verlaesst den unifix-Prozess nicht.
- Mieter-Session-Cookies werden gar nicht erst zu go2rtc
  weitergereicht (anderer Domain-Scope, Browser sendet sie
  ohnehin nicht im img-Request).
- Wenn go2rtc in einer spaeteren Saison einen eigenen Bearer-Mode
  bekommt, fuegt unifix den dann gezielt im Outgoing-Request hinzu;
  der Klartext-Token aus dem Endgeraet wird NIE direkt
  durchgereicht.

### 9.2 go2rtc-Admin nur via Session

Die /a/streams-CRUD-Endpoints laufen hinter `requireAdminSession`.
go2rtc selbst kennt keine Authentifikation - es ist nur sicher
solange es auf 127.0.0.1 gebunden ist. Operator-Pflicht:

- `api.listen` in go2rtc.yaml MUSS `127.0.0.1:1984` sein (siehe
  go2rtc.yaml.example im Repo-Root).
- Iptables / firewall darf 1984 nicht extern oeffnen.
- unifix-server bleibt einzige Frontline-Komponente.

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
unifix-eigenen AES-Wrapper-Schicht legen; fuer S14-01 reicht das
Loopback-Binding.
