// Workflow — die Prozessregeln, gerendert aus einer .md.
import { html } from "/app/util.js";
import { Markdown } from "/app/md.js";

export function Workflow() {
  const cfg = (window.COCKPIT_MANIFEST && window.COCKPIT_MANIFEST.workflow) || {};
  return html`<section class="split__main scroll scroll--wide">
    <${Markdown} url=${cfg.url} />
  </section>`;
}
