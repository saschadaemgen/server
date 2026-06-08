// Planung — To-Do-Board aus content/todos.js.
import { html, useState, cx } from "/app/util.js";

const PRIO = { hoch: "#f87171", mittel: "#fbbf24", niedrig: "#64748b" };

function Card({ card }) {
  const [open, setOpen] = useState(false);
  return html`<div class=${cx("card", "card--" + card.column)} onClick=${() => setOpen((o) => !o)}>
    <div class="card__top">
      <span class="card__prio" style=${{ background: PRIO[card.prio] || PRIO.niedrig }} title=${card.prio}></span>
      <span class="card__title">${card.title}</span>
      ${card.season ? html`<span class="card__season">${card.season}</span>` : null}
    </div>
    ${card.tags && card.tags.length
      ? html`<div class="card__tags">${card.tags.map((t) => html`<span key=${t} class="tag">${t}</span>`)}</div>`
      : null}
    ${card.note ? html`<div class=${cx("card__note", open && "card__note--open")}>${card.note}</div>` : null}
  </div>`;
}

export function Planung() {
  const data = window.COCKPIT_TODOS;
  if (!data) return html`<div class="md-status">todos.js fehlt.</div>`;
  return html`<div class="board scroll">
    ${data.columns.map((col) => {
      const cards = data.cards.filter((c) => c.column === col.id);
      return html`<div class="col" key=${col.id}>
        <div class="col__head">${col.label}<span class="col__n">${cards.length}</span></div>
        <div class="col__body">
          ${cards.length ? cards.map((c) => html`<${Card} key=${c.id} card=${c} />`) : html`<div class="col__empty">—</div>`}
        </div>
      </div>`;
    })}
  </div>`;
}
