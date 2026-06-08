# STREAM-CHAT-UEBERGABE-4 (Android-Subscriber-Pfad)

**Track:** Stream-Server-Chat Saison 4 (Cloud-Pfad: Android holt den Stream)
**Codename:** CARVILON (= Master-Saison 19)
**Vorgaenger:** Stream-Saison 3 (STREAM-SEASON-3-PROTOKOLL.md)
**Repo:** github.com/saschadaemgen/streaming-server (privat), branch `main`
**Stand bei Uebergabe:** Cloud-Medienpfad RPi<->VPS durch CGNAT bewiesen (ICE
connected), WHEP-Cold-Trigger + Egress-Auth gebaut. OFFEN: der ferne
Subscriber (Android) braucht eigene ICEServers - die reisen nicht im SDP.

> Selbsttragend fuer einen neuen Chat. Sprachpolitik: Code/Doku Englisch,
> Chat-Workflow Deutsch (JARVIS, "Sir").

---

## 0. Wer/Was (fuer einen neuen Chat ohne Vorgeschichte)

Der Stream-Server ist der **Medien-Layer**: zieht von der UniFi-Intercom
(RTSPS+SRTP), liefert Browser/Android (WebRTC, H.264 passthrough) + ESP (MJPEG
via ffmpeg). Bewusst DUMM (kennt Kameras + Profile, KEINE Mieter/Auth - die
sitzen im carvilon-server, Master-Chat). Cloud-Pfad: RPi (hinter CGNAT) ist
WHIP-CLIENT (outbound), die VPS ist WHIP-Ingress + WHEP-Egress mit Fan-Out.

## 1. Infrastruktur

```
RPi (edge):  192.168.1.42, ssh-Alias rpi, user sash710. carvilon-server
             -role=edge in-process (systemd USER carvilon-edge.service).
VPS (cloud): 194.164.197.247, ssh-Alias vps, /root/carvilon/. carvilon_stream-
             Build, in-process Cloud-Stream. :8444 WHIP-Ingress + WHEP-Egress,
             :8445 loopback request-publish-Hook (interim), nginx 443.
             TURN: UDP 3478 (+STUN), turns: 5349 auf einem public Hostnamen.
Kamera:      UA Intercom 679573e101080b03e4000424. UDM SE 192.168.1.1.
             API-Key/HMAC-Keys: in der Env, NIE committen.
```

## 2. Was am Cloud-Pfad STEHT (Saison 3, gebaut + bewiesen)

```
GEBAUT (streaming-server, lokal committet):
  - SetupCloudInProcess-Wrapper (Master verdrahtet ueber CloudSetupOptions).
  - In-process pion/turn: UDP-Relay + STUN (ein Port) + turns: (TLS, 5349,
    public Hostname + separates Cert). ICEMinter: Kurzzeit-REST-Creds/Peer.
  - WHEP-Cold-Trigger: kalter Subscriber -> request_publish (Open-Core-
    Callback OnRequestPublish -> Master sc.RequestPublish), 12s warten, 201.
  - Egress-Auth: WHEP verifiziert Bearer egress_token (eigener Key,
    fail-closed) VOR dem Cold-Trigger. publishtoken.Verify wiederverwendet.
  - TURN-Telemetrie: TURNStats() + OnTURNEvent + whipclient OnICEState
    (Open-Core, IP roh+maskiert, kein Secret).
  - Test-Werkzeuge: cmd/whep-probe (-token/-stun/-turn), cmd/mint-egress.

BEWIESEN:
  - ICE connected RPi<->VPS ueber UDP-Relay; turns: als TLS-Allocation auf 5349.
  - WHEP-Cold-Trigger-Kopplung: whep-probe -> request_publish(edges=1) -> RPi
    publiziert. (Subscriber-Medienpfad braucht client-ICEServers, s.u.)
```

Master-Naht (CloudSetupOptions-Felder): Addr, CertFile/KeyFile, HMACKey,
EgressHMACKey, TURNPublicIP/SharedSecret/Realm/UDPPort/TLSPort/PublicHost/
TLSCertFile/TLSKeyFile, OnRequestPublish, OnTURNEvent.

## 3. DER OFFENE BROCKEN = Saison-4-Kernthema: Android braucht eigene ICEServers

```
BEFUND (Saison 3): der WHEP-Subscriber-Medienpfad scheitert, wenn der Client
hinter NAT KEINE eigenen ICEServers hat - er bildet nur private host-Kandidaten.
URSACHE: ICEServers reisen NICHT im SDP (SDP traegt Kandidaten, nicht die
ICEServers-Konfig). Die Cloud-Seite ist in Ordnung (hat stun/turn/turns).
```

**Das ist der erste Saison-4-Auftrag: dem Subscriber ICEServers geben.**
Empfohlener Weg (ii, CARVILON-natuerlich): der **Master** gibt Android beim
Stream-Start stun/turn/turns + Kurzzeit-REST-Creds mit (via
stream.TURNCredentials + stream.TURNICEServers - existiert) - der symmetrische
Gegenpart zum request_publish-Frame des Edge. Alternative (i, Standard-WHEP):
ein `Link: rel="ice-server"`-Header am Egress + OPTIONS-Preflight (mehr
standardkonform, schwererer Client). Primaer **Master-Hoheit**.

## 4. Was Android konkret ansprechen wuerde (vom Android-Chat)

```
- WHEP-Subscribe: POST https://<vps-public>:8444/whep/<streamID>,
  Content-Type application/sdp, Authorization: Bearer <egress_token>.
  201 -> SDP-Answer; 401 ohne/falsches Token; 504 wenn der Edge nicht docked.
- BRAUCHT: (a) ein egress_token (Master stellt es aus, Endpoint steht), und
  (b) eigene ICEServers (Punkt 3). Heute :8444 = privates cloudca-Cert ->
  oeffentliche WHEP-URL ueber Hostname + public Cert ist Folgeposten.
```

## 5. Git-/Workflow-Regeln (strikt)

```
- CC committet LOKAL auf main, Conventional Commits, KEIN Branch/Worktree/Push.
- Sascha pusht manuell, koordiniert mit dem Master-Saison-Ende.
- Vor jedem Test: fragen welcher Server laeuft, Host/Port, frisch/alt. NIE
  zwei Server gleichzeitig an der Kamera.
- Briefings als .md, nie inline. MIT/Apache-Deps only, NIE AGPL. stdlib-Default.
- Token/API-Key/HMAC-Key/RTSPS-URL nie loggen/committen. Open-Core: KEIN
  pion-/Side-Channel-Typ an der stream-Naht (nur stdlib).
- low_delay NICHT wieder einbauen (D-0002, NoCodecLowDelay-Canary).
```

## 6. Erste Saison-4-Schritte (Vorschlag)

```
1. Git-Stand pruefen (main, Doku-Commits dieser Saison, Tree clean).
2. Mit dem Master abstimmen: Weg ii (Master gibt Android ICEServers beim
   Stream-Start) vs Weg i (WHEP-Link-Header). Empfehlung ii.
3. Falls Stream-Anteil: stream.TURNICEServers/TURNCredentials sind fertig -
   wahrscheinlich KEIN Stream-Bau noetig (Master-seitig). Sonst der Link-
   Header in handleWHEP (klein).
4. Android-SDP-Briefing giessen (POST /whep, Bearer egress_token, application/sdp).
5. End-to-end-Live-Test: Android (oder whep-probe -token -stun/-turn) zieht
   den Stream durch die VPS. Medienpfad + RTP bestaetigen.
6. Folgeposten: oeffentliche WHEP-URL ueber Hostname (public Cert).
```

## 7. Was Saison 4 NICHT tut

```
- Den MJPEG/ESP-Pfad NICHT anfassen (stabil).
- Die geteilte sourcereg.Registry NICHT umbauen.
- Die Quelle/Kamera NICHT aendern.
- Die Test-Werkzeuge NICHT in den Produktions-Build einbinden.
- low_delay NICHT wieder einbauen (D-0002).
- Kein Push, kein Branch, keine Version-Bumps.
```

## 8. Mit dem Master abzustimmen (koordiniertes Saison-Ende)

```
- Subscriber-ICEServers-Signalisierung: WER baut (Master Weg ii vs Stream
  Link-Header Weg i)? -> Sascha/Master-Entscheid.
- Die oeffentliche WHEP-Adresse (Hostname + Cert) fuer Android.
- JWS-zu-asymmetrisch-Swap der Token (gemeinsam, zentrale Krypto-Spec).
- Key-Rotation (publish + egress + TURN-Secret) vor Produktivbetrieb.
- Push-Reihenfolge Stream <-> Master am Saison-Ende.
```

---

**Uebergabe-Ende. Cloud-Medienpfad bewiesen, Egress-Auth + Cold-Trigger
stehen. Der eine offene Brocken: der ferne Subscriber (Android) braucht eigene
ICEServers - die reisen nicht im SDP, also gibt der Master sie beim
Stream-Start (Weg ii). Danach Android-SDP-Briefing + Live-Test. Master arbeitet
synchron. Erst messen/abstimmen, dann bauen.**
