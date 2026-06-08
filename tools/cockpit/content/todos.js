/*
 * CARVILON Cockpit — Planung / To-Dos (Board).
 *
 * Generische Season-Aufgaben. Pflege per Brief je Season. KEINE Secrets.
 * Abgeleitet aus den Saison-19/20-Notizen + Backlogs (Stand 2026-06-08).
 */
window.COCKPIT_TODOS = {
  columns: [
    { id: "backlog", label: "Backlog" },
    { id: "doing", label: "In Arbeit" },
    { id: "blocked", label: "Blockiert / Wurzel offen" },
    { id: "done", label: "Erledigt" },
  ],
  cards: [
    {
      id: "cloud-profile-keying",
      title: "Cloud-Profil-Keying-Bug",
      column: "blocked",
      prio: "hoch",
      tags: ["carvilon", "stream"],
      season: "S19/S20",
      note: "cloud_stream_profile wird ignoriert → immer intercom_web (Live: ~6 Mbit trotz intercom_med). Hypothese: Cloud-WHIP-Publish ist auf die Klingel/Doorbell-ID gekeyed, nicht auf die Viewer-ID → GetViewerInfo(streamID) findet den Viewer nicht → Default. Diskriminator: Edge-Restart → Profil des ersten frischen Publish.",
    },
    {
      id: "android-remote-video",
      title: "Android Remote-Video-Pfad (Cloud)",
      column: "doing",
      prio: "hoch",
      tags: ["stream"],
      season: "S20",
      note: "Cloud-Seite bewiesen (ICE erreicht connected über UDP-Relay). Offen: dem Remote-Client seine eigenen geminteten ICE-Server beim Stream-Start mitgeben (sie reisen nicht im SDP).",
    },
    {
      id: "doku-korrekturen",
      title: "Doku-Korrekturen S20 einarbeiten",
      column: "doing",
      prio: "mittel",
      tags: ["docs"],
      season: "S20",
      note: "Schema 19→24 + Migrationen 020–024 nachtragen; S19-Profil-Umbau (zwei Profil-Felder/Viewer) + offener Keying-Bug; in-process-Binary vs „merge deferred“ vereinheitlichen; go2rtc-Prämisse prüfen; Profil-Katalog 5 (Seed) klarstellen. Siehe Doku → Korrekturen & Stand S20.",
    },
    {
      id: "profile-catalog",
      title: "Profil-Katalog klären (5 Seed vs. „9 live“)",
      column: "backlog",
      prio: "mittel",
      tags: ["stream"],
      season: "S20",
      note: "Code-Seed = 5 (intercom_web, mjpeg_hq, mjpeg_bal, mjpeg_fast, h264_cbp). intercom_android/med/4g existieren NICHT als Seed-Profile — leben in Tests/Konzept. Quelle der „9 live“ verifizieren (ggf. CARVILON_PROFILES_JSON / DB).",
    },
    {
      id: "ports-doku",
      title: "Port-Realität dokumentieren",
      column: "backlog",
      prio: "niedrig",
      tags: ["stream", "docs"],
      season: "S20",
      note: "Verifiziert: Side-Channel :8443 (carvilon Master), WHIP+WHEP :8444, optional öffentl. WHEP :8446, TURN 3478/5349. :8447 existiert nirgends. In die Docs eintragen, widersprüchliche Stellen bereinigen.",
    },
    {
      id: "cockpit-iterate",
      title: "Cockpit nach Saschas Review iterieren",
      column: "doing",
      prio: "mittel",
      tags: ["cockpit"],
      season: "S20",
      note: "Erste lauffähige Schale steht (5 Tabs, Bauplan + Szenario-Playback, Doku/Archiv rendern echte .md). Danach: Feinschliff nach Review.",
    },
    {
      id: "monorepo-merge",
      title: "Monorepo-Merge (carvilon + stream)",
      column: "done",
      prio: "hoch",
      tags: ["infra"],
      season: "S20",
      note: "Beide Histories grafted (kein Squash), go.work im Root. Kanon: C:\\Projects\\Carvilon\\server.",
    },
    {
      id: "per-viewer-cloud-profile",
      title: "Per-Viewer Cloud-Stream-Profil (Migration 024)",
      column: "done",
      prio: "mittel",
      tags: ["carvilon"],
      season: "S19",
      note: "Zweites Profil-Feld je Viewer (LAN + Cloud), Spalte cloud_stream_profile, Admin-UI-Selects. Wirkung noch durch den Keying-Bug blockiert.",
    },
  ],
};
