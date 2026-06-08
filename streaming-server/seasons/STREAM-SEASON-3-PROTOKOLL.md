# STREAM-SERVER SAISON 3 PROTOKOLL

**Track:** Stream-Server-Chat (eigener Video-Stream-Layer, MIT)
**Codename:** CARVILON (= Master-Saison 18)
**Repo:** github.com/saschadaemgen/streaming-server (privat)
**Modulpfad:** carvilon.local/stream
**Endstand:** Cloud-Medienpfad RPi<->VPS durch CGNAT bewiesen (ICE/STUN/TURN
gebaut + connected), WHEP-Cold-Trigger verdrahtet, TURN-Telemetrie-Datenquelle,
Egress-Auth (eigener Key). Test-Werkzeuge whep-probe + mint-egress.

> Sprachpolitik: Code/Doku Englisch, Chat-Workflow Deutsch (JARVIS, "Sir").
> Neben-Track zum Master (carvilon-Server) und ESP (Innenmonitor).

---

## Was diese Saison war

Saison 2 hatte die Cloud-Naht gebaut (WHIP-Ingress live, Token-Durchstich),
aber EIN grosser Brocken blieb offen: **ICE/STUN/TURN** - RPi hinter CGNAT und
VPS fanden keinen Medienpfad (checking->failed nach 30s). Saison 3 hat genau
diesen Brocken geloest und den Cloud-Pfad bis zur Aussen-Zugangs-Auth
fertiggebaut.

**Kernfragen der Saison:**
- Wie kommt der Medienpfad RPi<->VPS durch CGNAT zustande?
- Wie loest ein ferner Client (Android) den Stream aus, ohne den loopback-Hook?
- Wie sieht der Admin ICE/TURN-Telemetrie (ohne pion an der offenen Naht)?
- Wie sperrt man den WHEP-Egress (sonst zieht jeder den Stream)?

**Ergebnis:** Alle vier beantwortet + gebaut. ICE connected (UDP-Relay + STUN +
turns:). WHEP-Cold-Trigger koppelt den Subscriber an request_publish. TURN-
Telemetrie als Open-Core-Datenquelle. Egress-Auth mit eigenem HMAC-Key,
fail-closed, 401 vor dem Trigger.

---

## DIE GROSSEN ERKENNTNISSE (Gold fuer die Zukunft)

### 1. RPi hinter CGNAT braucht TURN, nicht nur STUN
srflx allein verbindet nicht durch CGNAT - ein Relay (TURN) ist Pflicht. STUN
laeuft gratis auf demselben Relay-UDP-Port mit (pion beantwortet Binding
unauthenticiert). pion/turn eingebettet, EIN Server traegt UDP+TLS. (D-0005)

### 2. turns: braucht Hostname + public Cert - eine private CA verifiziert pion nicht
pion prueft den turns:-TLS-Handshake gegen den System-Root-Pool mit
ServerName = URL-Host. Ein bare-IP-turns mit privater cloudca-CA wird
abgelehnt. Loesung: turns: auf einem oeffentlichen Hostnamen mit separatem
public Cert; WHIP behaelt sein cloudca-Cert. (D-0005)

### 3. Ein Relay-ICE-Kandidat ist immer "udp" - das ist KEIN Bug
Der Kandidat wird nach dem RELAYTEN Transport (udp) benannt, egal ob der Client
den Relay ueber udp oder tls/tcp erreicht. "Kein proto=tcp-Kandidat" ist
erwartet; die zwei Relay-Kandidaten = turn:udp + turns:tls. (Kostete einen
ganzen Befund.)

### 4. ICEServers reisen NICHT im SDP
Der SDP-Answer traegt die gegatherten Kandidaten des Servers, nicht seine
ICEServers-Konfiguration. Ein NAT-Client braucht seine EIGENEN ICEServers, um
srflx/relay zu bilden. Der Subscriber-Pfad scheiterte am Tool ohne eigene
ICEServers - kein Server-Bug. Das ist die offene Android-Signalisierung. (D-0005)

### 5. Push und Pull brauchen GETRENNTE Keys
Egress-Token byte-identisch zum publish_token, aber mit eigenem Key signiert.
Ein publish-Key-Token am Egress -> signature mismatch -> 401. Verhindert, dass
ein Publish-Recht ein Pull-Recht wird. Verify wiederverwendet, kein neuer
Krypto-Code. (D-0008)

### 6. Open-Core haelt: kein pion/Side-Channel-Typ an der Naht
Telemetrie (TURNStats/TURNEvent/ICEStateEvent) und der WHEP-Trigger
(RequestPublishFunc) tragen nur stdlib-Typen. pions net.Addr wird nur an der
Grenze gelesen und zu Strings gerendert. Der Master verdrahtet alles ueber
CloudSetupOptions, ohne dass das stream-Paket den Side-Channel kennt. (D-0006/7)

---

## Was gebaut wurde (Commits, lokal)

```
e82c9e8  icedebug: maskierbares ICE-Kandidaten-/State-Logging (CARVILON_ICE_DEBUG)
e2b5f76  SetupCloudInProcess-Wrapper (Spiegel der Edge-Naht)
d405fe9  pion/turn eingebettet, ICEMinter fuer Cloud-Peers (soft-gated)
12dc0a9  whipclient akzeptiert TURN-ICEServers beim Publish
a91071e  credential-lose STUN-Zeile neben TURN in ICEServers
1e7432b  TLS-Relay 5349 (turns:) im selben pion-Server, soft-gated
9741167  turns: auf public Hostname + getrenntes TLS-Cert
e8ab4e1  TURNStats-Getter + OnTURNEvent-Lifecycle-Callback (Telemetrie)
def8357  whipclient OnICEState strukturierter Callback
5bf8937  WHEP-Cold-Trigger: request_publish bei kaltem Subscribe (Open-Core-Callback)
b44b0c8  Egress-Auth: 401-Verify-Branch in handleWHEP (eigener Key)
5b88140  cmd/whep-probe Test-Hilfsmittel (WHEP-Cold-Subscribe)
20c043f  whep-probe -stun/-turn ICE-Flags
f2072a7  whep-probe -token Flag (Egress-Auth-Test)
1de8481  cmd/mint-egress Test-Hilfsmittel (Egress-Token minten)
```
Frueh in der Saison (ESP-fps, Strang 3b): be9e7a2 (S2-16 low_delay raus),
a522f84 (S3-01 even -r 30 + fast_bilinear), 351a3b2 (D-0002), 6cd8bf3 (D-0004).

## Die Test-Werkzeuge (NICHT im Produktions-Build)
- **cmd/whep-probe**: echter pion-WHEP-Subscriber. Flags -url, -token, -stun/
  -turn, -hold, -insecure. Loggt HTTP-Status + ICE-State + RTP-Ankunft.
- **cmd/mint-egress**: mintet ein Token im publish/egress-Format. Key NUR aus
  Env (nie geloggt), stdout NUR das Token. Round-Trip gegen publishtoken.Verify
  bewiesen.
Beide eigenes package main, vom Edge-/Cloud-Binary nicht mitgezogen (verifiziert).

## Drei-Chat-Stand am Saison-Ende
- **Master (S18):** verdrahtet CloudSetupOptions (HMACKey, EgressHMACKey, TURN-
  Felder, OnRequestPublish, OnTURNEvent). Egress-Ausstellung + Endpoint stehen.
- **Android:** wartet auf die Subscriber-ICEServers-Signalisierung (Weg ii,
  Master gibt ICEServers beim Stream-Start) + die oeffentliche WHEP-Adresse.
- **ESP:** unberuehrt von Saison 3 (MJPEG-Pfad stabil; S3-01 half dem ESP-Bild).

## Offene Posten (Saison 4 / Master-19)
Subscriber-ICEServers fuer Android (das Gating-Item, D-0005); oeffentliche
WHEP-URL ueber Hostname; Android-SDP-Briefing; JWS-zu-asymmetrisch-Swap
(gemeinsam mit publish_token); Egress-Rate-Limit + Client-Bindung; Key-Rotation
vor Produktivbetrieb (Hygiene). Siehe stream-server-feature-backlog.md +
stream-server-security.md.

---

## Bewertung

Saison 3 hat den in Saison 2 offen gebliebenen Cloud-Medienpfad bewiesen und
den Aussen-Zugang abgesichert. Marathon-Disziplin: jeder ICE/turns-Befund erst
gemessen (Kandidaten-Typen, Cert-Trust, das "udp-Label"-Missverstaendnis, die
SDP-ICEServers-Erkenntnis), dann gebaut. Open-Core sauber gehalten. Naechste
Saison gehoert dem Android-Subscriber-Pfad.

**Erst messen, dann bauen. Open-Core haelt. Push am Saison-Ende, koordiniert
mit dem Master.**

**Geschaeftsgeheimnis. Sascha-intern. Stand: Ende Stream-Saison 3.**
