# unifix Architecture

**Status:** Saison 10, lebendes Dokument, wird pro Saison ergaenzt.
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

## 5. Lebenszyklus eines Mock-Geraets

1. Admin im unifix-server-UI: "Geraet anlegen"
2. Pool-Manager spawnt mock-Subprozess mit eigener MAC/IP/Port
3. Mock antwortet auf UDM-Multicast-Discovery (Stage 1)
4. Admin in UniFi-UI: "Verwenden"
5. UDM schickt Adoption-Push an Mock:8080 (Stage 4)
6. Mock baut WS (Stage 5) und MQTT (Stage 6) auf
7. UDM sieht Mock als "online" UA Intercom Viewer
8. Admin in unifix-server-UI: Mock einem Mieter zuordnen
9. unifix-server generiert Magic-Link-UUID, Sascha gibt sie an Mieter

## 6. Lebenszyklus eines Klingel-Events

1. Besucher drueckt an echter UA Intercom den Klingel-Knopf
2. UDM publisht MQTT-RPC /remote_view an zugeordneten Mock
3. Mock-RPC-Handler reicht Event an unifix-server weiter
4. unifix-server pusht via SSE oder Long-Poll an Mieter-Endgeraet
5. Mieter sieht Live-Bild, klickt "Tuer auf"
6. Endgeraet sendet POST /api/v1/doors/<id>/unlock an unifix-server
7. unifix-server proxied via PUT /api/v1/developer/doors/<id>/unlock
   gegen die UniFi Access Developer-API auf der UDM (Alternative:
   Mock-RPC fuer Test-Setups ohne offizielle API)
8. UDM oeffnet via UA Hub Door die echte Tuer

## 7. Saisons-Roadmap (Go-Aera)

Siehe CLAUDE.md Sektion 15. Kurz:

```
Saison 10:  Skelett + Server-Pool + Mock-Stages + Smoketest
Saison 11:  Klingel-Lifecycle komplett dekodiert
            Mock kann selbst Tueren oeffnen
            Mediaserver "ms" identifiziert (RTSPS 7441, LiveFLV 7550)
            Offizielle UniFi Access Developer-API entdeckt
            STATUS: abgeschlossen 12. Mai 2026
Saison 12:  Mieter-Browser-UI, Magic-Link, Klingel im Browser
            STRATEGISCHE MAXIME: maximal Original-API uebernehmen
            (UniFi Access Developer API v4.2.16 als Hauptquelle)
            - HTTPS-Client gegen /api/v1/developer/* fuer Mieter-CRUD
            - Webhook-Empfang von access.doorbell.incoming
            - RTSPS-Stream-Empfang Port 7441 via go2rtc-Bridge
Saison 13:  ESP-Endgeraete-Anbindung
Saison 14:  Lizenz-Server-Fleisch, CA pro Lizenz, TLS-Klient
Saison 15+: Hardware-Bindung, Plattform-Erweiterungen
```

## 8. Was unifix NICHT ist

- Kein UniFi-Replacement (UDM + Hub Door + echte Kamera bleiben Pflicht)
- Kein Open-Source-Projekt
- Kein Hardware-Hersteller (wir liefern Software, optional Convenience-RPi)
- Kein Sicherheits-Garantie-Geber (siehe security.md)
- Keine Cloud-Pflicht (alles lokal moeglich, Cloud-Update optional)
