# ADR-STREAM-01: Integration carvilon-streaming-server in carvilon-server

**Status:** ENTSCHIEDEN (21. Mai 2026)
**Beteiligt:** Sascha (Entscheider), Master-Chat, Stream-Chat
**Bezug:** FRAGE/ANTWORT-STREAM-01/02/03
**Zweck:** Festhalten WIE der closed-source Streaming-Server in den
(kuenftig oeffentlichen) carvilon-server integriert wird, OHNE das
Open-Core-Modell zu brechen. Damit spaeter - auch fuer einen
Nachfolge-Claude - nichts durcheinanderkommt.

---

## 1. Das Spannungsfeld

```
- carvilon-server: soll OEFFENTLICH werden (Open Core). Jeder muss
  ihn bauen koennen, OHNE Zugang zu geheimem Code.
- carvilon-streaming-server: CLOSED SOURCE, kommerzielles Produkt.
  Repo privat (github saschadaemgen/streaming-server, Module-Pfad
  carvilon.local/stream).
- Ziel Endkunde: EIN Binary, ein Prozess, "laeuft einfach"
  (fertiges RPi-Image).
- Konflikt: ein oeffentliches Repo, das ein privates HART
  importiert, ist von aussen NICHT baubar -> Open Core verbrannt.
```

---

## 2. ENTSCHEIDUNG: Interface-Naht + Build-Tags (ein Binary)

Industriestandard fuer Open-Core (GitLab, Grafana, Sentry machen es
so). Quellcode-Linking ueber ein Interface, NICHT Binary-Einbettung
(verworfene Alternative siehe Abschnitt 6).

```
- Das OEFFENTLICHE carvilon-Repo definiert NUR das StreamBackend-
  Interface PLUS eine triviale Default-Implementierung:
     -> liefert klaren "kein Backend"-Zustand (HTTP 503).
  Damit ist das oeffentliche Repo OHNE privaten Code voll baubar.

- Der PRIVATE carvilon-streaming-server implementiert dasselbe
  Interface.

- Einlinkung NUR im KOMMERZIELLEN Build, ueber Go Build-Tag
  (z.B. //go:build carvilon_stream) oder separaten main-Wrapper.
  Der oeffentliche Build importiert das private Package NIE.

- Ergebnis: EIN Binary fuer den Endnutzer. Zwei Bau-Wege:
     oeffentlich (ohne Tag) -> Default-Impl (503), kein Geheimnis
     kommerziell (mit Tag)  -> echtes Stream-Backend eingelinkt

- Bild: das Freie kennt nur die NAHT (Interface), das Kommerzielle
  FUELLT sie. Quellcode-Linking, kein eingebettetes Binary.
```

### Warum dieser Weg

```
- Ein echtes einzelnes Programm, ein Prozess, direkte Aufrufe
  (kein Inter-Prozess-Geraffel).
- Sauberer Cross-Compile fuer arm64 (ein Go-Build).
- Keine Virenscanner-Probleme (kein Programm das ein anderes
  auspackt/startet).
- Lizenz-/Limit-Check sitzt im closed Stream-Build, wo ihn niemand
  auskommentieren kann. carvilon-Kern kennt kein Limit; Stream-
  Backend reicht bei N>Limit einen Fehler durch.
```

---

## 3. Das StreamBackend-Interface (vollstaendig, drei Methodengruppen)

Das Interface lebt im OEFFENTLICHEN carvilon-Repo und ist die
EINZIGE Vertrags-Grenze. Aus den Schnittstellen-Antworten folgt,
dass es breiter ist als die alte reine go2rtc-Stream-Anbindung:

```
1. STREAM nach Profil liefern
   - der Proxy-Pfad: ESP-MJPEG + Browser-WebRTC, adressiert per
     Profilname.

2. PROFIL-CRUD
   - fuer /a/streams: strukturierte Profile anlegen, lesen,
     aendern, loeschen.

3. KAMERA-LISTE holen (ListCameras)
   - fuer das Profil-Dropdown. Der Stream-Server hat den
     Protect-API-Key, der freie carvilon-server bekommt KEINEN
     eigenen Protect-Zugang.
```

Aenderungen an dieser Grenze NUR abgestimmt zwischen Master-Chat
und Stream-Chat, nie einseitig.

---

## 4. Profil-Modell (strukturiert, kein go2rtc-String mehr)

```
- go2rtc-Transcode-String faellt weg.
- Profil = strukturierte Felder:
     Name        menschenlesbar (z.B. "esp_low", "browser_hd")
     Kamera      camera-id (UI zeigt Klarnamen via ListCameras)
     Qualitaet   hoch / mittel / niedrig
                 -> mappt 1:1 auf Protect-API rtsps-stream
                    qualities high/medium/low
     Verwendung  browser (WebRTC) / esp (MJPEG)
   FPS/Aufloesung/Transport leitet der Stream-Server INTERN ab.
- Alle drei Qualitaetsstufen sind ABRUFBAR (Vorschau klein, Viewer
  mittel, Vollbild hoch), aber nur die ANGESCHAUTE laeuft
  (Lifecycle pro Stufe, schont RPi).
- stream_profile-Spalte der Viewer BLEIBT. Wert ist jetzt ein
  strukturierter Profilname statt go2rtc-Source-String.
  -> KEIN DB-Migrations-Zwang (Spalte unveraendert, nur Semantik
     des Inhalts aendert sich).
- ResolveStreamProfile-Logik (type=esp -> esp-Profil, type=web ->
  browser-Profil) bleibt als Auswahl-Mechanik.
- Aufnehmen/Recording: NICHT in diesem Modell. Eigene spaetere
  Saison (Backlog) - DSGVO-Aufbewahrung, Speicher, Loeschfristen
  sind zu gewichtig fuer den Stream-Kern.
```

---

## 5. Routen-Hoheit + UI

```
- Route /a/streams BLEIBT (vertraut fuer den Nutzer). Eine
  Oberflaeche, kein eigener Routes-Satz fuer den Stream-Server.
- handler_admin_streams.go bleibt als UI-Schicht; ihr Innenleben
  wird vom go2rtc-Aufruf auf das StreamBackend-Interface getauscht.
- UI-Konzept (drei Einstellungen, einfache Ansicht vs Profil-
  Ansicht): KONZEPT-STREAM-PROFILE-UI. Wird ERST nach S3 zum
  Implementierungs-Briefing.
```

---

## 6. VERWORFENE Alternative: fertiges Binary einbetten

Festgehalten, damit die Entscheidung nachvollziehbar bleibt und
nicht erneut diskutiert wird.

```
IDEE (Saschas erster Gedanke): Das fertige, limitierte Stream-
Binary per Go embed in den oeffentlichen Build einbacken. Ein File
fuer den Kunden, Geheimnis bleibt als kompiliertes Stueck drin.

FUNKTIONIERT - aber in der Praxis seltener gewaehlt, weil:
- Eingebettetes Binary muss zur Laufzeit ausgepackt und als
  ZWEITER Prozess gestartet werden -> Inter-Prozess-Kommunikation
  noetig, mehr Komplexitaet als ein direkter Aufruf.
- Cross-Compile umstaendlich: das eingebettete Binary muss fuer
  JEDE Zielplattform (arm64, amd64) separat vorliegen.
- Virenscanner schlagen bei Programmen an, die andere Programme
  auspacken und starten.

Der Interface-Naht-Weg (Abschnitt 2) vermeidet all das: ein echtes
einzelnes Programm, ein Prozess, sauberer Cross-Compile.

ENTSCHEIDUNG: Interface-Naht gewaehlt. Binary-Einbettung verworfen.
Beide ergeben ein Binary fuer den Kunden - die Naht ist technisch
glatter.
```

---

## 7. Wer baut was (Reihenfolge + Zustaendigkeit)

```
STREAM-CHAT (dieser Chat):
   S2 Fan-Out         DURCH (6 Viewer live, ein Pull verifiziert)
   S3 MJPEG-Output    laeuft (Decode-Recherche zuerst)
   dann: Befuellen der Naht (privater Stream-Server implementiert
         StreamBackend) - eigenes Briefing, sobald MJPEG steht.

MASTER-CHAT (Saison 15):
   - etabliert die StreamBackend-Interface-Naht im OEFFENTLICHEN
     Repo (die drei Methodengruppen + Default-Impl mit 503).
   - go2rtc-Schicht (handler_admin_streams.go, internal/streams/,
     buildBackendStreamURL, Hijack-Proxy) wird NICHT mehr
     feingeputzt - sie ist Wegwerf, sobald die Naht steht.

go2rtc-AUSBAU:
   - erst wenn MJPEG-Ausgang verifiziert ist.
   - dann: Proxy STREAM_BACKEND_URL auf den eingebauten Stream-
     Server umbiegen.
   - bis dahin bleibt go2rtc fuer den ESP-Pfad am Leben.
```

---

## 8. Merksatz fuer spaeter

```
Das Freie kennt nur die Naht. Das Kommerzielle fuellt sie.
Ein Interface im oeffentlichen Repo, ein Build-Tag fuers private
Package, ein Binary fuer den Kunden. Profile sind strukturiert,
nicht go2rtc-Strings. go2rtc faellt erst nach MJPEG.
```
