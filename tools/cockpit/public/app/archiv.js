// Archiv — Season-Protokolle + Übergaben, BEIDE Doku-Sätze (carvilon + stream)
// + Projekt-Saison-Dokumente, dynamisch via /api/list geladen und nach Saison
// sortiert.
import { html, useState, useEffect, useMemo, cx, fetchJSON } from "/app/util.js";
import { Markdown } from "/app/md.js";

function classify(name) {
  const m = name.match(/(\d+)/);
  const season = m ? parseInt(m[1], 10) : null;
  const up = name.toUpperCase();
  let type = "Doku";
  if (up.includes("PROTOKOLL")) type = "Protokoll";
  else if (up.includes("UEBERGABE") || up.includes("ÜBERGABE") || up.includes("HANDOVER")) type = "Übergabe";
  else if (up.includes("INDEX")) type = "Index";
  else if (up.includes("BACKLOG")) type = "Backlog";
  else if (up.includes("KONZEPT")) type = "Konzept";
  else if (up.includes("ANLEITUNG")) type = "Anleitung";
  return { season, type };
}

export function Archiv() {
  const cfg = (window.COCKPIT_MANIFEST && window.COCKPIT_MANIFEST.archiv) || { roots: [] };
  const [entries, setEntries] = useState(null);
  const [active, setActive] = useState(null);
  const [error, setError] = useState(null);

  useEffect(() => {
    let live = true;
    Promise.all(
      cfg.roots.map((rc) =>
        fetchJSON(`/api/list?root=${encodeURIComponent(rc.root)}`)
          .then((files) =>
            files.map((f) => {
              const { season, type } = classify(f.name);
              return { ...rc, name: f.name, size: f.size, season, type, url: `/docs/${rc.root}/${encodeURIComponent(f.name)}` };
            }),
          )
          .catch(() => []),
      ),
    ).then((lists) => {
      if (!live) return;
      const all = lists.flat();
      if (!all.length) setError("Keine Saison-Dateien gefunden (Doc-Roots evtl. nicht vorhanden — siehe Server-Log).");
      setEntries(all);
    });
    return () => {
      live = false;
    };
  }, []);

  const groups = useMemo(() => {
    if (!entries) return [];
    const setOrder = Object.fromEntries(cfg.roots.map((r, i) => [r.set, i]));
    const bySeason = new Map();
    for (const e of entries) {
      const key = e.season == null ? "ref" : e.season;
      if (!bySeason.has(key)) bySeason.set(key, []);
      bySeason.get(key).push(e);
    }
    const seasons = [...bySeason.keys()].filter((k) => k !== "ref").sort((a, b) => b - a);
    const order = seasons.map((s) => ({ key: s, label: "Saison " + s, items: bySeason.get(s) }));
    if (bySeason.has("ref")) order.push({ key: "ref", label: "Referenz & Index", items: bySeason.get("ref") });
    for (const grp of order) {
      grp.items.sort((a, b) => (setOrder[a.set] - setOrder[b.set]) || a.type.localeCompare(b.type) || a.name.localeCompare(b.name));
    }
    return order;
  }, [entries]);

  if (error && !entries?.length) return html`<div class="md-status md-status--error">${error}</div>`;
  if (!entries) return html`<div class="md-status">Archiv lädt …</div>`;

  return html`<div class="split">
    <nav class="split__side scroll">
      ${groups.map(
        (grp) => html`<div class="navgroup" key=${grp.key}>
          <div class="navgroup__title">${grp.label}</div>
          ${grp.items.map(
            (it) => html`<button
              key=${it.url}
              class=${cx("navitem", "navitem--arch", active === it.url && "navitem--on")}
              onClick=${() => setActive(it.url)}
            >
              <span class="navitem__badge" style=${{ background: it.color }}>${it.set}</span>
              <span class="navitem__type">${it.type}</span>
            </button>`,
          )}
        </div>`,
      )}
    </nav>
    <section class="split__main scroll">
      ${active
        ? html`<${Markdown} url=${active} key=${active} />`
        : html`<div class="archiv__welcome">
            <h2>Archiv</h2>
            <p class="muted">${entries.length} Dokumente aus ${cfg.roots.length} Quellen. Eintrag links wählen.</p>
          </div>`}
    </section>
  </div>`;
}
