### Observability-Dashboard (Stream-Auslastung + Stau-Erkennung)

```
Sascha-Wunsch 21. Mai (Stream-Server-Chat). Live-Dashboard im
CARVILON-Admin, das den Zustand des Stream-Servers sichtbar macht.

Anzeige-Bereiche:
   - Auslastung pro Kamera: wie viele Viewer haengen dran, laeuft
     die Kamera ueberhaupt (bedarfsgesteuert), grobe Bandbreite
   - Stau-Erkennung: Drop-Rate pro Client. Verliert ein Viewer
     viele Frames -> seine Leitung/sein Geraet ist ueberlastet
     -> rote Markierung "dieser Viewer hinkt hinterher"
   - System-Last: Server-CPU, Anzahl laufender ffmpeg-Prozesse
     (MJPEG-Transcode), Naehe zum Hardware-Limit (RPi-Free-Tier)

Technische Grundlage (schon halb da):
   - Der Stream-Server zaehlt intern bereits: Subscriber pro Hub
     (total=N), Drop-Counter pro langsamem Client, Pull-Status
     pro Kamera, Bildraten. Heute landen die Zahlen nur im Log.
   - Saubere Loesung: Server stellt Metriken ueber einen kleinen
     Endpoint bereit (Prometheus-Format ueblich), Dashboard liest
     + zeichnet. Server misst, Dashboard zeigt - getrennt.
   - Kann ueber dieselbe StreamBackend-Interface-Naht laufen wie
     der Rest.

Einordnung: eigenes Feature, NICHT Teil der jetzigen Integrations-
phase. Sinnvoll NACHDEM die Naht steht und go2rtc raus ist.
Passt zum Premium-Gedanken (Hausverwalter-Monitoring) und zur
Live-Grid-Demo (gleicher Marketing-Kontext: Multi-Tenant sichtbar
machen).

Saison 16+ oder wenn der eigene Stream-Server produktiv laeuft.
```
