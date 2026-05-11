# unifix Security Plan

**Status:** Saison 10, lebendes Dokument.
**Stand:** Strategische Eckpunkte gesetzt, technische Umsetzung
ueberwiegend in spaeteren Saisons.
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

Saison 12-13:  TLS-Layer mit selbst-signiertem Cert, Fingerprint
               beim Erstkontakt vom Mieter akzeptiert (Wireguard-Stil)

Saison 14+:    TLS mit Kunden-Eigen-CA, vom Lizenz-Server ausgegeben.
               Browser-Warnungen behebbar, ESP-Klient kann
               Cert-Pinning machen.
```

#### 2.2.4 API-Token-Sicherheit

Saison 12+ verwendet die offizielle UniFi Access Developer API
(siehe wire-format.md und CLAUDE.md Sektion 21). Auth ist
API-Key-Header oder Bearer-Token, generiert im UniFi Portal vom
Anlagen-Admin.

Das API-Token gibt VOLLEN Zugriff auf User-CRUD, Door-Unlock,
Doorbell-Trigger usw. Es muss daher:

- niemals im Browser oder Endgeraet landen
- niemals in Logs oder Error-Reports erscheinen
- niemals in Saison-Protokollen oder Goldminen-Files persistieren
- nur im unifix-server-Process-Speicher leben, am besten in einer
  read-only-config geladen beim Start
- pro Anlage einmalig vom Admin gesetzt werden, nicht generierbar
  vom unifix-server selbst

Der Browser/Endgeraet-Klient redet ausschliesslich mit dem
unifix-server (eigener Magic-Link), nicht direkt mit der UDM-API.

### 2.3 Lizenz-Server-Seite (RPi <-> Cloud)

TLS PFLICHT ab Tag eins der Saison 14. Lizenz-Validierung ohne
TLS ist trivial spoofbar. Konkret:

- Cloud-Server hat Let's-Encrypt-Cert oder Eigen-CA-Root
- RPi-Client prueft Cert-Validitaet und ggf. Pin
- Lizenz-Schluessel sind asymmetrisch signiert, nicht nur
  Bearer-Tokens

## 3. Lizenz- und Hardware-Bindung (Saison 14+)

Beschluss Saison-10-Abend: jede Lizenz wird an die RPi-Hardware
gebunden. Mehrere Stufen, von einfach zu hart:

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

### 4.3 Optional in Saison 14: garble (Go-Obfuskator)

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
