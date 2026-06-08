// Cockpit shell — Header, Tab-Navigation, sanfte Tab-Übergänge, Bootstrap.
import { createRoot } from "/vendor/lib.js";
import { html, useState, Icon, cx } from "/app/util.js";
import { Bauplan } from "/app/bauplan.js";
import { Doku } from "/app/doku.js";
import { Planung } from "/app/planung.js";
import { Workflow } from "/app/workflow.js";
import { Archiv } from "/app/archiv.js";

const TABS = [
  { id: "bauplan", label: "Bauplan", icon: "bauplan", comp: Bauplan },
  { id: "doku", label: "Doku", icon: "doku", comp: Doku },
  { id: "planung", label: "Planung", icon: "planung", comp: Planung },
  { id: "workflow", label: "Workflow", icon: "workflow", comp: Workflow },
  { id: "archiv", label: "Archiv", icon: "archiv", comp: Archiv },
];

function App() {
  const [tab, setTab] = useState("bauplan");
  const meta = (window.SYSTEM_MAP && window.SYSTEM_MAP.meta) || {};
  const Active = (TABS.find((t) => t.id === tab) || TABS[0]).comp;
  return html`<div class="app">
    <header class="topbar">
      <div class="brand">
        <span class="brand__mark"></span>
        <span class="brand__name">CARVILON</span>
        <span class="brand__sub">Cockpit</span>
      </div>
      <nav class="tabs">
        ${TABS.map(
          (t) => html`<button key=${t.id} class=${cx("tab", tab === t.id && "tab--on")} onClick=${() => setTab(t.id)}>
            <${Icon} name=${t.icon} size=${17} /><span>${t.label}</span>
          </button>`,
        )}
      </nav>
      <div class="topbar__stand" title=${meta.note || ""}>${meta.stand || ""}</div>
    </header>
    <main class="stage">
      <div class="view" key=${tab}><${Active} /></div>
    </main>
  </div>`;
}

const el = document.getElementById("root");
el.classList.remove("boot");
el.textContent = "";
createRoot(el).render(html`<${App} />`);
