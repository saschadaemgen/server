/*
 * CARVILON Cockpit — Inhalts-Manifest (Tabs -> Dateien).
 *
 * Mappt die Tabs auf die ECHTEN Repo-Docs. Die Living-Docs (carvilon-*) sind
 * gitignored und werden vom Server unter /docs/<root>/ an ihren bestehenden
 * lokalen Pfaden gelesen — NICHT hierher kopiert (eine Quelle der Wahrheit).
 * Die /content/notes/*.md sind getrackte, generische Kuratierungs-Notizen.
 *
 * Pflege per Brief je Season -> Seite zeigt es sofort (kein Rebuild).
 */
window.COCKPIT_MANIFEST = {
  doku: [
    {
      title: "Übersicht & Stand",
      items: [
        { label: "Cockpit-Übersicht", url: "/content/notes/overview.md" },
        { label: "Korrekturen & Stand S20", url: "/content/notes/corrections-s20.md" },
      ],
    },
    {
      title: "Carvilon Core — Living Docs",
      hint: "gitignored · echte Anlagen-Daten · nur lokal",
      items: [
        { label: "Architektur", url: "/docs/carvilon-docs/carvilon-server-architecture.md" },
        { label: "Security", url: "/docs/carvilon-docs/carvilon-server-security.md" },
        { label: "Wire-Format", url: "/docs/carvilon-docs/carvilon-server-wire-format.md" },
        { label: "Decisions", url: "/docs/carvilon-docs/carvilon-server-decisions.md" },
        { label: "Feature-Backlog", url: "/docs/carvilon-docs/carvilon-server-feature-backlog.md" },
      ],
    },
    {
      title: "Streaming-Server — Living Docs",
      hint: "getrackt im Monorepo",
      items: [
        { label: "Architektur", url: "/docs/stream-docs/stream-server-architecture.md" },
        { label: "Decisions", url: "/docs/stream-docs/stream-server-decisions.md" },
        { label: "Wire-Format", url: "/docs/stream-docs/stream-server-wire-format.md" },
        { label: "Security", url: "/docs/stream-docs/stream-server-security.md" },
        { label: "Setup-Notes", url: "/docs/stream-docs/stream-server-setup-notes.md" },
        { label: "Feature-Backlog", url: "/docs/stream-docs/stream-server-feature-backlog.md" },
      ],
    },
  ],

  workflow: { url: "/content/notes/workflow.md" },

  // Archiv lädt die Saison-Dateien dynamisch via /api/list?root=… und sortiert
  // sie nach Saison-Nummer. BEIDE Doku-Sätze (carvilon + stream) + die
  // Projekt-Saison-Dokumente.
  archiv: {
    roots: [
      { root: "project-docs", set: "Projekt (aktuell)", color: "#f59e0b" },
      { root: "carvilon-seasons", set: "Carvilon", color: "#22c55e" },
      { root: "stream-seasons", set: "Streaming", color: "#a855f7" },
    ],
  },
};
