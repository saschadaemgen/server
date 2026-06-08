// Doku — gerenderte Living-Docs mit Sidebar-Navigation (aus dem Manifest).
import { html, useState, useMemo, cx } from "/app/util.js";
import { Markdown } from "/app/md.js";

export function Doku() {
  const groups = (window.COCKPIT_MANIFEST && window.COCKPIT_MANIFEST.doku) || [];
  const flat = useMemo(() => groups.flatMap((g) => g.items), [groups]);
  const [active, setActive] = useState(flat.length ? flat[0].url : null);

  if (!flat.length) return html`<div class="md-status">Kein Doku-Manifest gefunden.</div>`;

  return html`<div class="split">
    <nav class="split__side">
      ${groups.map(
        (g) => html`<div class="navgroup" key=${g.title}>
          <div class="navgroup__title">
            ${g.title}${g.hint ? html`<span class="navgroup__hint">${g.hint}</span>` : null}
          </div>
          ${g.items.map(
            (it) => html`<button
              key=${it.url}
              class=${cx("navitem", active === it.url && "navitem--on")}
              onClick=${() => setActive(it.url)}
            >${it.label}</button>`,
          )}
        </div>`,
      )}
    </nav>
    <section class="split__main scroll">
      <${Markdown} url=${active} key=${active} />
    </section>
  </div>`;
}
