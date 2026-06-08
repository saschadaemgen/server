/*
 * CARVILON — System-Bauplan (datengetrieben).
 *
 * EINE Quelle der Wahrheit fuer das Diagramm. window.SYSTEM_MAP wird von der
 * Schale zur Laufzeit gerendert (Auto-Layout via dagre). Pflege per Brief je
 * Season -> Seite zeigt es sofort (kein Rebuild).
 *
 * SICHERHEIT: ausschliesslich GENERISCHE Labels. KEINE echten IPs / MACs /
 * UUIDs / Hostnamen / Secrets. Port-Nummern sind Konfig-Konventionen (keine
 * Secrets) und gegen den echten Code verifiziert (Saison 20).
 *
 * Schema:
 *   groups : [{ id, label, color }]
 *   nodes  : [{ id, label, group, role, ports?:[string] }]
 *   edges  : [{ from, to, label, direction:'uni'|'bi', kind:'control'|'media'|'data', pathTag? }]
 *   scenarios : [{ id, label, color, steps:[{ node, edge?:[from,to], text }] }]
 */
window.SYSTEM_MAP = {
  meta: {
    title: "CARVILON — System-Bauplan",
    stand: "Saison 20 · 2026-06-08",
    note: "Generische Sicht. Ein Binary (in-process Stream via Build-Tag carvilon_stream); Cloud ist additiv (Ausfall blockiert die Edge nie).",
  },

  groups: [
    { id: "UniFi", label: "UniFi", color: "#3b82f6" },
    { id: "RPi-Edge", label: "RPi Edge (lokal, offline-fähig)", color: "#22c55e" },
    { id: "streaming-server", label: "streaming-server (in-process)", color: "#a855f7" },
    { id: "VPS-Cloud", label: "VPS Cloud (additiv)", color: "#f59e0b" },
    { id: "Clients", label: "Clients", color: "#14b8a6" },
  ],

  nodes: [
    // ── UniFi ────────────────────────────────────────────────────────────
    { id: "unifi-intercom", label: "UniFi Intercom (Türklingel + Kamera)", group: "UniFi", role: "Klingel am Eingang; trägt Kamera/Audio." },
    { id: "unifi-controller", label: "UniFi Controller (UDM)", group: "UniFi", role: "UDM mit UniFi Access + Protect; publiziert Doorbell-RPCs, treibt die Tür." },
    { id: "unifi-door", label: "UniFi Tür-Hub / Relais", group: "UniFi", role: "Access-Türschloss; öffnet auf Controller-Befehl." },

    // ── RPi-Edge (carvilon-server, role=edge — ein Binary) ───────────────
    { id: "edge-http", label: "Edge HTTP / Admin / WebViewer", group: "RPi-Edge", role: "Steuer-Gehirn: APIs, Auth, SSE-Hub, Persistenz.", ports: ["HTTP :9080"] },
    { id: "viewer-manager", label: "Viewer-Manager (Mock-Viewers)", group: "RPi-Edge", role: "Eine Goroutine je Viewer; emuliert einen UniFi-Intercom-Viewer zum Controller." },
    { id: "doorbell-hub", label: "Doorbell-Hub (Fan-out)", group: "RPi-Edge", role: "Routet Klingel-Events per Viewer-MAC an SSE / Eventbus / FCM." },
    { id: "ua-client", label: "UniFi-Access-Client", group: "RPi-Edge", role: "Ruft die Access Developer-API (Bearer); löst Tür auf und entriegelt." },
    { id: "fcm-sender", label: "FCM Push-Sender", group: "RPi-Edge", role: "Edge-seitiger Firebase-Client; Data-Push direkt an die Android-App (nicht über Cloud)." },
    { id: "sidechannel-edge", label: "Side-Channel-Client (mTLS)", group: "RPi-Edge", role: "Outbound mTLS-WebSocket durch CGNAT zur Cloud; trägt Steuer-Frames." },
    { id: "edge-db", label: "Edge-DB (SQLite)", group: "RPi-Edge", role: "Lokaler Store: viewers, sessions, doorbell_calls, door_events, Pro-Viewer-Settings.", ports: ["Schema 24"] },

    // ── streaming-server (in-process auf der Edge; Build-Tag) ─────────────
    { id: "stream-source", label: "Stream-Source (RTSPS-Pull + Depacketize)", group: "streaming-server", role: "Zieht die Kamera (RTSPS, opt. SRTP/SDES) vom UDM, depacketisiert H.264." },
    { id: "stream-hub", label: "Fan-out-Hub (1 Decode → N Encodes)", group: "streaming-server", role: "Lazy: Source startet beim ERSTEN Subscriber; IDR-Cache." },
    { id: "mjpeg-enc", label: "MJPEG-Encoder", group: "streaming-server", role: "ffmpeg-Subprozess; MJPEG für das ESP-Wandpanel.", ports: ["Stream :8555"] },
    { id: "webrtc-lan", label: "WebRTC-Egress (LAN, H.264 passthrough)", group: "streaming-server", role: "pion WebRTC ohne Re-Encode für Browser/App im LAN.", ports: ["Stream :8555"] },
    { id: "whip-pub", label: "WHIP-Publisher (→ Cloud)", group: "streaming-server", role: "Outbound WebRTC-Publish des Streams zur Cloud, wenn ein Remote-Viewer zieht." },

    // ── VPS-Cloud (gleiches Binary, role=cloud; additiver Bridge) ────────
    { id: "reverse-proxy", label: "Reverse-Proxy (TLS-Front)", group: "VPS-Cloud", role: "TLS-Terminierung vor der Cloud.", ports: [":443"] },
    { id: "sidechannel-server", label: "Side-Channel-Server (mTLS · Signaling)", group: "VPS-Cloud", role: "Terminus der Steuer-Frames; mintet kurzlebige ICE-Creds; triggert Edge-Publish.", ports: ["Side-Channel :8443"] },
    { id: "whip-ingress", label: "WHIP-Ingress", group: "VPS-Cloud", role: "Nimmt den Edge-WHIP-Publish entgegen.", ports: [":8444"] },
    { id: "cloud-streamhub", label: "Cloud-Streamhub (Fan-out, kein Re-Encode)", group: "VPS-Cloud", role: "Verteilt den publizierten Stream byte-agnostisch an WHEP-Subscriber." },
    { id: "whep-egress", label: "WHEP-Egress (öffentlich)", group: "VPS-Cloud", role: "Browser-vertrauter öffentlicher Listener für Remote-WHEP-Pulls; prüft Egress-Token.", ports: [":8444", "opt. :8446"] },
    { id: "turn", label: "TURN / STUN-Relay", group: "VPS-Cloud", role: "In-process TURN/STUN, damit CGNAT-Edge und Remote-Client einen Medienpfad finden.", ports: ["UDP :3478", "TLS :5349"] },

    // ── Clients ──────────────────────────────────────────────────────────
    { id: "android-app", label: "Android-App", group: "Clients", role: "Mieter-Telefon (Bearer). Klingelt via FCM; Video LAN-direkt (WHEP) oder über Cloud; Unlock/Answer/Reject." },
    { id: "web-viewer", label: "Browser-WebViewer", group: "Clients", role: "Mieter-Browser (Session-Cookie). Klingel über SSE; WebRTC-Stream; Unlock-POST." },
    { id: "esp-module", label: "ESP Tür-Modul", group: "Clients", role: "ESP32-Wandpanel (Bearer) — die Offline-Garantie. Klingel/Stream/Unlock rein LAN-lokal." },
  ],

  edges: [
    // ── Doorbell (Klingel) ───────────────────────────────────────────────
    { from: "unifi-intercom", to: "unifi-controller", label: "Klingeldruck", direction: "uni", kind: "control", pathTag: "doorbell" },
    { from: "unifi-controller", to: "viewer-manager", label: "Doorbell-RPC (/remote_view)", direction: "bi", kind: "control", pathTag: "doorbell" },
    { from: "viewer-manager", to: "doorbell-hub", label: "Event (Key = Viewer-MAC)", direction: "uni", kind: "control", pathTag: "doorbell" },
    { from: "doorbell-hub", to: "web-viewer", label: "SSE doorbell_start", direction: "uni", kind: "control", pathTag: "doorbell" },
    { from: "doorbell-hub", to: "esp-module", label: "Eventbus/SSE (LAN, offline)", direction: "uni", kind: "control", pathTag: "doorbell" },
    { from: "doorbell-hub", to: "fcm-sender", label: "FCM-Leg", direction: "uni", kind: "control", pathTag: "doorbell" },
    { from: "fcm-sender", to: "android-app", label: "FCM Data-Push (weckt App)", direction: "uni", kind: "control", pathTag: "doorbell" },
    { from: "doorbell-hub", to: "edge-db", label: "door_events (Audit)", direction: "uni", kind: "data" },

    // ── Unlock (Tür öffnen) ──────────────────────────────────────────────
    { from: "web-viewer", to: "edge-http", label: "POST …/unlock", direction: "uni", kind: "control", pathTag: "unlock" },
    { from: "esp-module", to: "edge-http", label: "POST /esp/unlock", direction: "uni", kind: "control", pathTag: "unlock" },
    { from: "android-app", to: "edge-http", label: "POST unlock (LAN)", direction: "uni", kind: "control", pathTag: "unlock" },
    { from: "edge-http", to: "ua-client", label: "Tür auflösen + Unlock", direction: "uni", kind: "control", pathTag: "unlock" },
    { from: "ua-client", to: "unifi-controller", label: "Access-API Unlock (Bearer)", direction: "bi", kind: "control", pathTag: "unlock" },
    { from: "unifi-controller", to: "unifi-door", label: "Tür öffnen", direction: "uni", kind: "control", pathTag: "unlock" },

    // ── Edge-intern ──────────────────────────────────────────────────────
    { from: "edge-http", to: "edge-db", label: "viewers / sessions / settings", direction: "bi", kind: "data" },
    { from: "viewer-manager", to: "edge-db", label: "viewer-Zeilen + Tür-Pairings", direction: "bi", kind: "data" },

    // ── Video LAN-direkt ─────────────────────────────────────────────────
    { from: "unifi-intercom", to: "stream-source", label: "Kamera RTSPS (TLS, opt. SRTP)", direction: "uni", kind: "media", pathTag: "video-lan" },
    { from: "stream-source", to: "stream-hub", label: "decoded AU", direction: "uni", kind: "media", pathTag: "video-lan" },
    { from: "stream-hub", to: "mjpeg-enc", label: "Fan-out-Tap", direction: "uni", kind: "media", pathTag: "video-lan" },
    { from: "stream-hub", to: "webrtc-lan", label: "Fan-out-Tap (passthrough)", direction: "uni", kind: "media", pathTag: "video-lan" },
    { from: "mjpeg-enc", to: "esp-module", label: "GET /api/stream.mjpeg (LAN)", direction: "uni", kind: "media", pathTag: "video-lan" },
    { from: "web-viewer", to: "edge-http", label: "POST /webviewer/offer (SDP)", direction: "bi", kind: "control", pathTag: "video-lan" },
    { from: "edge-http", to: "webrtc-lan", label: "Offer proxien + Auth", direction: "bi", kind: "control", pathTag: "video-lan" },
    { from: "webrtc-lan", to: "web-viewer", label: "WebRTC (DTLS-SRTP)", direction: "bi", kind: "media", pathTag: "video-lan" },
    { from: "webrtc-lan", to: "android-app", label: "WebRTC via edge_whep_url (LAN zuerst)", direction: "uni", kind: "media", pathTag: "video-lan" },

    // ── Video Cloud-relayed ──────────────────────────────────────────────
    { from: "sidechannel-edge", to: "sidechannel-server", label: "mTLS-WebSocket (outbound, CGNAT)", direction: "bi", kind: "control", pathTag: "control" },
    { from: "android-app", to: "whep-egress", label: "POST /whep/{id} (Egress-Token)", direction: "bi", kind: "control", pathTag: "video-cloud" },
    { from: "whep-egress", to: "cloud-streamhub", label: "Subscriber → Cold-Publish", direction: "uni", kind: "control", pathTag: "video-cloud" },
    { from: "sidechannel-server", to: "sidechannel-edge", label: "request_publish + ICE-Creds", direction: "uni", kind: "control", pathTag: "video-cloud" },
    { from: "sidechannel-edge", to: "whip-pub", label: "start publish", direction: "uni", kind: "control", pathTag: "video-cloud" },
    { from: "stream-hub", to: "whip-pub", label: "Fan-out-Tap (H.264, kein Re-Encode)", direction: "uni", kind: "media", pathTag: "video-cloud" },
    { from: "whip-pub", to: "reverse-proxy", label: "POST /whip/{id} (outbound)", direction: "bi", kind: "media", pathTag: "video-cloud" },
    { from: "reverse-proxy", to: "whip-ingress", label: "TLS terminieren", direction: "uni", kind: "media", pathTag: "video-cloud" },
    { from: "whip-pub", to: "turn", label: "ICE/Relay (3478 / 5349)", direction: "bi", kind: "media", pathTag: "video-cloud" },
    { from: "whip-ingress", to: "cloud-streamhub", label: "Publisher-RTP → Fan-out", direction: "uni", kind: "media", pathTag: "video-cloud" },
    { from: "cloud-streamhub", to: "whep-egress", label: "Subscriber drainen", direction: "uni", kind: "media", pathTag: "video-cloud" },
    { from: "android-app", to: "turn", label: "ICE/Relay (eigene ICE-Server)", direction: "bi", kind: "media", pathTag: "video-cloud" },
    { from: "whep-egress", to: "android-app", label: "WHEP-Media (DTLS-SRTP via Relay)", direction: "bi", kind: "media", pathTag: "video-cloud" },
  ],

  scenarios: [
    {
      id: "doorbell",
      label: "Klingel",
      color: "#f59e0b",
      summary: "Besucher drückt die Klingel → Browser, ESP und Android klingeln (Control-Pfad, kein Video).",
      steps: [
        { node: "unifi-intercom", text: "Besucher drückt die Klingel am UniFi-Intercom." },
        { node: "unifi-controller", edge: ["unifi-intercom", "unifi-controller"], text: "Intercom meldet die Klingel an den UDM-Controller." },
        { node: "viewer-manager", edge: ["unifi-controller", "viewer-manager"], text: "Controller publiziert die Doorbell-RPC an den adoptierten Mock-Viewer (Routing per Viewer-MAC)." },
        { node: "doorbell-hub", edge: ["viewer-manager", "doorbell-hub"], text: "Viewer feuert das Event in den Doorbell-Hub (Key = Viewer-MAC)." },
        { node: "web-viewer", edge: ["doorbell-hub", "web-viewer"], text: "Browser-Leg: SSE doorbell_start → Klingel-Overlay." },
        { node: "esp-module", edge: ["doorbell-hub", "esp-module"], text: "ESP-Leg: Eventbus/SSE — funktioniert im LAN ganz ohne Internet (Offline-Garantie)." },
        { node: "fcm-sender", edge: ["doorbell-hub", "fcm-sender"], text: "FCM-Leg: Hub übergibt an den Edge-FCM-Sender." },
        { node: "android-app", edge: ["fcm-sender", "android-app"], text: "Data-only-Push direkt an Firebase → Telefon wacht aus Doze auf und klingelt." },
      ],
    },
    {
      id: "unlock",
      label: "Tür öffnen",
      color: "#22c55e",
      summary: "Mieter tippt „Tür öffnen“ → Edge löst die Tür auf → UniFi entriegelt. Reiner Control-Pfad.",
      steps: [
        { node: "android-app", text: "Mieter tippt „Tür öffnen“ (App / Browser-Overlay / ESP gleichwertig)." },
        { node: "edge-http", edge: ["android-app", "edge-http"], text: "Client POSTet den Unlock an die Edge (LAN direkt; remote über den Cloud-Control-Relay an denselben Handler)." },
        { node: "ua-client", edge: ["edge-http", "ua-client"], text: "Edge löst die Tür auf (direkte Door-ID mit Authz, zugewiesene Tür oder gepaartes Intercom)." },
        { node: "unifi-controller", edge: ["ua-client", "unifi-controller"], text: "Access Developer-API: Door-Unlock (Bearer) an den Controller." },
        { node: "unifi-door", edge: ["unifi-controller", "unifi-door"], text: "Controller treibt das Tür-Relais → die Tür öffnet." },
      ],
    },
    {
      id: "video-lan",
      label: "Video LAN",
      color: "#3b82f6",
      summary: "Viewer im LAN → Stream direkt von der Edge, ohne Cloud. Lazy Fan-out.",
      steps: [
        { node: "unifi-intercom", text: "Intercom-Kamera liefert H.264." },
        { node: "stream-source", edge: ["unifi-intercom", "stream-source"], text: "Edge zieht RTSPS (TLS; SRTP/SDES optional) und depacketisiert." },
        { node: "stream-hub", edge: ["stream-source", "stream-hub"], text: "Lazy Fan-out-Hub: Source startet beim ERSTEN Subscriber (1 Decode → N Encodes)." },
        { node: "webrtc-lan", edge: ["stream-hub", "webrtc-lan"], text: "Tap zum WebRTC-Egress (H.264 passthrough, kein ffmpeg)." },
        { node: "web-viewer", edge: ["webrtc-lan", "web-viewer"], text: "Browser: Signaling /webviewer/offer über die Edge, Medien direkt per WebRTC — kein Cloud-Weg." },
        { node: "esp-module", edge: ["mjpeg-enc", "esp-module"], text: "Parallel ESP-Pfad: MJPEG-Encoder → GET /api/stream.mjpeg im LAN." },
      ],
    },
    {
      id: "video-cloud",
      label: "Video Cloud",
      color: "#a855f7",
      summary: "Remote-Viewer → Signaling/Relay über die Cloud (WHIP/WHEP/TURN). Cold-Publish bei Subscriber-Ankunft.",
      steps: [
        { node: "android-app", text: "Remote-Viewer (hinter NAT/Mobilfunk) will den Stream." },
        { node: "whep-egress", edge: ["android-app", "whep-egress"], text: "Viewer POSTet /whep/{id} mit Egress-Token (fail-closed Prüfung vor jedem Cold-Trigger)." },
        { node: "sidechannel-edge", edge: ["sidechannel-server", "sidechannel-edge"], text: "Noch kein Publisher → Cloud schickt request_publish (+ICE-Creds) über den Side-Channel zur Edge." },
        { node: "whip-pub", edge: ["sidechannel-edge", "whip-pub"], text: "Edge startet den WHIP-Publisher und tappt den Fan-out-Hub (H.264, kein Re-Encode)." },
        { node: "whip-ingress", edge: ["whip-pub", "reverse-proxy"], text: "Edge publiziert outbound (CGNAT-freundlich) über den Reverse-Proxy an den WHIP-Ingress." },
        { node: "turn", edge: ["whip-pub", "turn"], text: "ICE: CGNAT-Edge relayt via TURN (srflx allein verbindet nicht)." },
        { node: "cloud-streamhub", edge: ["whip-ingress", "cloud-streamhub"], text: "Publisher-RTP landet im Cloud-Streamhub (Fan-out)." },
        { node: "android-app", edge: ["whep-egress", "android-app"], text: "WHEP-Media fließt über ICE/DTLS-SRTP (via Relay) zum Telefon → Video läuft." },
      ],
    },
  ],
};
