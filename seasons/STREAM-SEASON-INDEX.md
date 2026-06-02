# STREAM-SERVER SEASON INDEX

**Track:** Stream-Server-Chat (eigener Video-Stream-Layer, MIT)
**Codename:** CARVILON
**Repo:** github.com/saschadaemgen/streaming-server (privat)
**Stand:** Stream-Saison 3 abgeschlossen (Cloud-Medienpfad bewiesen,
Egress-Auth + Telemetrie); Saison 4 = Android-Subscriber-Pfad

> Sprachpolitik: Code/Doku Englisch, Chat-Workflow Deutsch.
> Neben-Track zum Master (carvilon-Server) und ESP (Innenmonitor).
> Sascha mediiert zwischen den drei Chats.

---

## Quick status

```
DONE  Saison 1 (22.-25. Mai 2026)
   go2rtc-ABLOESUNG vollzogen. Eigener MIT-Server zieht RTSPS+SRTP
   von der UniFi-Intercom und liefert WebRTC (Browser) + MJPEG (ESP).
   Alle drei Wege gruen: Browser/WebViewer, Admin, ESP.
   SRTP/SDES geknackt (war kein MIKEY) als einschaltbares Feature.
   Latenz auf go2rtc-Niveau, Schlieren weg, Stats fuer alle Codecs.
   go2rtc komplett vom RPi entfernt.

DONE  Saison 2 (29.-31. Mai 2026)
   Doppel-Saison. (1) CLOUD-NAHT gebaut: VPS-WHIP-Ingress deployt +
   live, Token-Durchstich bestanden, Steuerpfad end-to-end bewiesen.
   Einzig offen: ICE/STUN/TURN (RPi hinter CGNAT + VPS finden noch
   keinen Medienpfad) -> Saison 3a. (2) ESP-MJPEG-BILDREGRESSION
   geloest: `-flags +low_delay` schaltete Multi-Core-Decode ab, bei
   1200x1600 fiel SW-Decode hinter Echtzeit zurueck, P-Frame-Verlust,
   GOP-105-Smear. Flag raus -> 18 Min live ohne Drop, ffmpeg 184% ->
   7,5% CPU. Doku-Muster erweitert: 3 Ist-Docs + 1 decisions-Log.

DONE  Saison 3 (= Master-Saison 18)
   - 3a Cloud-Weiterbau GELOEST: ICE/STUN/TURN durch CGNAT. In-process
     pion/turn (UDP-Relay + STUN ein Port + turns: TLS 5349 auf public
     Hostnamen). ICE connected, turns: bestaetigt. WHEP-Cold-Trigger
     (Subscriber -> request_publish, Open-Core-Callback). Egress-Auth
     (eigener HMAC-Key, fail-closed, 401 vor dem Trigger). TURN-
     Telemetrie (TURNStats + OnTURNEvent + OnICEState, Open-Core).
     Test-Werkzeuge cmd/whep-probe + cmd/mint-egress.
   - 3b ESP-fps (S3-01, a522f84): fps 12->20-25, ESP ~11->~13, kein
     Verpixeln. D-0003/D-0004.
   - OFFEN -> Saison 4: der ferne Subscriber (Android) braucht eigene
     ICEServers (reisen nicht im SDP) -> Master gibt sie beim Stream-
     Start. Handover STREAM-CHAT-HANDOVER-4.md.

LATER Saison 3+/4+: ROADMAP / KONZEPT
   Zwei-Wege-Audio (Tuer-Ton leicht, Zuschauer-Ton zur Tuer ist
   eigenes RE-Thema). Verkaufs-Features (Wasserzeichen, Event-
   Aufnahme an Klingel, Live-Grid). Drei-Tier-Quellprofile (hoch/
   mittel/niedrig umschaltbar, Registry hat Quality im Pull-Key).
   Lizenz-Limit + mehrere Kunden-Anlagen. Stream-Server in EINEN
   carvilon-Binary (GOPRIVATE, bewusst verschoben). Multi-Kamera.
```

---

## Season overview

### Saison 1 (22.-25. Mai 2026) - DONE
**Protokoll:** STREAM-SEASON-1-PROTOKOLL.md
**Erreicht:** go2rtc abgeloest. Eigener Go-Server (gortsplib +
pion/webrtc + ffmpeg-Subprozess + modernc/sqlite, alles MIT). RTSPS-
Pull mit eigenem RFC-6184-Depacketizer (UniFi sendet Mode-2 trotz
Mode-1-Deklaration). Fan-Out lazy + geteilt. MJPEG fuer ESP, WebRTC
fuer Browser, H.264-CBP-Transcode. SRTP/SDES selbst entschluesselt
(stdlib-Krypto, kein pion/srtp wegen Paketlimit). encryption global
steuerbar. Live-Stats fuer alle Codecs inkl. WebRTC. Auf dem RPi
deployed. 24 Commits, alle gepusht.
**Schluessel-Erkenntnisse:** SDES statt MIKEY; Latenz = zu grosser
Puffer (go2rtc-Vergleich); Schlieren = burstiges Verwerfen statt
gleichmaessigem fps-Sampling; Verschluesselung gehoert an die Quelle,
nicht ans Profil.

### Saison 2 (29.-31. Mai 2026) - DONE
**Protokoll:** STREAM-SEASON-2-PROTOKOLL.md
**Erreicht:** Doppel-Saison - Cloud-Naht + ESP-Fix.

**(1) Cloud-Naht (local-first), Strang Edge/Cloud:**
- Edge-Rolle schiebt EINEN H.264-Stream lazy per WHIP nach oben.
- Cloud-Rolle (VPS `194.164.197.247`) nimmt an, faechert per pion
  broadcast (TrackLocalStaticRTP) an WHEP-Subscriber, kein Re-Encode.
- VPS-WHIP-Ingress (`:8444`) deployt + live, systemd
  `carvilon-stream-cloud`, nginx 443, Cert 0600 root.
- Token-Durchstich bestanden (Bearer end-to-end, rohes SDP rein/raus).
- OFFEN: ICE/STUN/TURN - checking->failed nach 30s, kein Medienpfad
  durch CGNAT. HMAC verifiziert identisch (NICHT die Ursache).
  Loesungsweg: pion/turn auf VPS 443/TCP, STUN, SDP-Answer lokal+
  Cloud-Kandidaten. -> Saison 3a.

**(2) ESP-MJPEG-Bildregression (low_delay):**
- Symptom: MJPEG (ESP + Browser) ruckelte mit wachsender Latenz/
  Verpixeln bei Bewegung; WebRTC immer sauber.
- 2 Tage Diagnose, viele Sackgassen (alle in decisions D-0002
  dokumentiert, NICHT wiederholen): nicht B-Frames, nicht Decode-
  Fehler, nicht S2-Code, nicht CPU, nicht In-Process-Merge, nicht
  Kamera-/UDM-Update.
- WURZEL: `-flags +low_delay` (S6-07-Geburtsfehler) schaltet Multi-
  Core-Frame-Threading des H.264-Decoders ab. Bei 1200x1600 High
  Profile fiel Single-Thread-SW-Decode hinter Echtzeit zurueck,
  encoder-input staute, P-Frame-Verlust, GOP-105 = ~5s Smear pro
  Verlust. WebRTC immun (passthrough, kein Decode).
- FIX: low_delay raus (be9e7a2). 18 Min live, frames_dropped 0,
  ffmpeg 184% -> 7,5% CPU. HW-Decode v4l2m2m gemessen 1.6x LANGSAMER
  als threaded SW, daher nicht genutzt. Doku 351a3b2 (D-0002).
**Schluessel-Erkenntnisse:** WebRTC sauber + MJPEG kaputt zeigt IMMER
auf den ffmpeg-Transcode; encoder-input-Drops = Decode kommt nicht
nach (kaputtes Bild), viewer-frames-Drops = Client holt nicht ab
(Transport, Bild intakt); alten funktionierenden Git-Stand wieder-
herstellen schlaegt zehn Hypothesen.

### Saison 3 (= Master-Saison 18) - DONE
**Protokoll:** STREAM-SEASON-3-PROTOKOLL.md
**Handover:** STREAM-CHAT-HANDOVER-4.md (Android-Subscriber-Pfad)

Drei Straenge, alle im Stream-Server-Chat gefuehrt:

**3a - Cloud-Weiterbau (GELOEST):** ICE/STUN/TURN durch CGNAT. In-process
pion/turn (UDP-Relay + STUN ein Port + turns: TLS 5349 auf public Hostnamen
mit separatem Cert). ICE connected, turns: bestaetigt. WHEP-Cold-Trigger
(Subscriber -> request_publish, Open-Core-Callback). Egress-Auth (eigener
HMAC-Key, fail-closed, 401 vor dem Trigger). TURN-Telemetrie (TURNStats +
OnTURNEvent + OnICEState, Open-Core, IP roh+maskiert). Befunde: ein Relay-
Kandidat ist immer udp; ICEServers reisen nicht im SDP; turns braucht public
Cert. Detail: stream-server-decisions.md D-0005..D-0008. Test-Werkzeuge:
cmd/whep-probe + cmd/mint-egress. OFFEN -> Saison 4: Android braucht eigene
ICEServers (Master gibt sie beim Stream-Start).

**3b - Server-seitige ESP-fps-Optimierung (S3-01 DONE):**
- Befund: MJPEG lief nur bei 12 fps sauber; 15/20 verpixelten sofort,
  OBWOHL ffmpeg nur ~7% CPU (kein Rechenproblem). Reproduziert im
  Browser = rein server-seitig.
- Wurzel (gemessen am camera-dump, isoliert): (a) `-use_wallclock_as_
  timestamps 1` stempelte burstige Pipe-Ankunftszeit als PTS -> fps-
  Filter sampelte klumpig -> encoder-input-Queue lief bei hoeheren fps
  ueber. (b) Default-Scaler (bicubic) war der Durchsatz-Wuerger.
- FIX S3-01 (a522f84): wallclock raus, `-r 30` + `-threads 4` vor -i,
  `scale=...:flags=fast_bilinear`. Messung: fps=20 von 1.18x (staute)
  auf 2.61x, fps=25 auf 2.42x. Live: ESP von ~11 auf ~13 fps, sauber,
  frames_dropped 0, ffmpeg 8% CPU, WebRTC unberuehrt. Canary
  NoWallclockEvenRate bewacht Rueckfall. Doku D-0003.
- Kamera verifiziert 30 fps (UniFi-UI + ffprobe 1200,1600 hoher Tier).
  Wir ziehen den hohen Tier (WebRTC braucht ihn), Optimierung
  funktioniert MIT dem hohen Tier.

**ESP-Transport-Hebel (an ESP-Chat uebergeben):** Warum nur ~13 statt
20 fps am ESP ankommen, obwohl Server 20 liefert und ESP zu 83-93%
idle ist: der TCP-Empfang am ESP ist der Engpass (viewer-frames-Drops
am Server). Befund + Fix an ESP-Chat: setsockopt SO_RCVBUF ~256K +
TCP_NODELAY nach connect(), recv_buf 32K->256K (stream_pipeline.c
Z.507), optional lwIP-sdkconfig. ESP-Chat arbeitet, Sascha vermittelt.

**Offene Folgeposten (spaeter):**
- `h264esp/encoder.go` haengt noch auf wallclock + Default-Scaler
  (ungenutzt, h264_cbp nicht in Default-Profilen). Symmetrischer Fix
  falls je genutzt.
- Drei-Tier-Quellprofile (hoch/mittel/niedrig umschaltbar). MJPEG
  koennte mittleren Tier (960x1280) ziehen fuer noch mehr Reserve,
  WebRTC hohen. Registry hat Quality im Pull-Key, vorbereitet.

### Saison 3+/4+ (Konzept)
Verkaufs-Features (Wasserzeichen = doppelter Hebel Premium + Gratis-
Limit; Event-Aufnahme an der Klingel = Alleinstellung gegenueber
generischen Stream-Servern; Live-Grid fuer Hausverwalter/Demo).
Lizenz-Limit + Mandanten-faehige Mehr-Kunden-Versorgung. Zwei-Wege-
Audio (Tuer-Ton via vorhandenem Audio-Track; Zuschauer-Ton zur Tuer
als eigenes RE-Thema, da unklar ob UA-Intercom fremden Input annimmt).
Binaries-Zusammenlegung + GOPRIVATE, wenn beide Produkte stabil sind.
Multi-Kamera (Protect AI 360 etc.).

---

## Commit-Stand (streaming-server main, lokal)

```
Saison 3 - Cloud-Arc (lokal committet):
1de8481  chore cmd/mint-egress (Egress-Token minten)
f2072a7  chore whep-probe -token Flag
b44b0c8  feat Egress-Auth: 401-Verify in handleWHEP (eigener Key)
5bf8937  feat WHEP-Cold-Trigger: request_publish (Open-Core-Callback)
def8357  feat whipclient OnICEState Callback
e8ab4e1  feat TURNStats + OnTURNEvent (Telemetrie)
9741167  feat turns: public Hostname + getrenntes Cert
1e7432b  feat TLS-Relay 5349 (turns:) soft-gated
a91071e  feat STUN-Zeile neben TURN
12dc0a9  feat whipclient nimmt TURN-ICEServers
d405fe9  feat pion/turn eingebettet + ICEMinter
e2b5f76  feat SetupCloudInProcess-Wrapper
e82c9e8  feat icedebug (maskierbares ICE-Logging)
20c043f  chore whep-probe -stun/-turn   |  5b88140  chore cmd/whep-probe
Frueh (3b ESP-fps): 6cd8bf3 D-0004, a522f84 S3-01, 351a3b2 D-0002, be9e7a2 S2-16
+ diese Saison-Abschluss-Doku-Commits.
```
Push am Saison-Ende, Saschas Wort, KOORDINIERT mit dem Master-Saison-Ende.
Sascha pusht zwischen den Zuegen (origin folgt laufend). stash@{0}
(S2-09+S2-13) weiter unberuehrt - erst auf Saschas Wort droppen.

---

## Konventionen (Stream-Track)

```
- Briefings als .md via create_file+present_files, nie inline
  (nested code blocks brechen Chat-Rendering). Naming BRIEFING-
  STREAM-SXX-NN bzw CC-BRIEFING-SXX-NN; ANTWORT/SYNC/FRAGE-AN-
  MASTER-CHAT bzw -AN-ESP-CHAT.
- Claude Code committet lokal, pusht NIE. Sascha pusht manuell.
  Push-Disziplin: nach jedem groesseren Meilenstein.
- CC liest IMMER echten Code gegen, statt aus Briefing abzutippen.
  Erst Befund melden, dann aendern. Messen statt raten.
- Drei Straenge ab Saison 3 alle im Stream-Chat (kein separater
  3a/3b-Chat). ESP-Firmware ist EIGENER Chat, Sascha vermittelt.
- stdlib-Default. Neue Dep nur wenn noetig, nur MIT/Apache, kein AGPL.
- Mess-Regel: VOR jedem Test fragen, ob/welcher Server laeuft, auf
  welchem Host/Port, frisch oder alt. Nie annehmen, dass ein Server
  beendet ist. Nie zwei Server/ffmpeg gleichzeitig an der Kamera.
- Token/Key/RTSPS-URL nie loggen/committen.
```

---

**Geschaeftsgeheimnis. Sascha-intern. Stand 2026-05-31.**
