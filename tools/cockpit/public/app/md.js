// Markdown rendering of the living docs. Content is local, trusted (Sascha's
// own repo docs) and served same-origin, so marked's HTML output is rendered
// directly. If untrusted content is ever added, add a sanitizer here.
import { marked } from "/vendor/lib.js";
import { html, useState, useEffect } from "/app/util.js";

marked.setOptions({ gfm: true, breaks: false });

export function renderMarkdown(text) {
  return marked.parse(text || "");
}

export function Markdown({ url, text }) {
  const [state, setState] = useState({ loading: !!url, html: "", error: null });

  useEffect(() => {
    if (text != null) {
      setState({ loading: false, html: renderMarkdown(text), error: null });
      return;
    }
    if (!url) return;
    let live = true;
    setState({ loading: true, html: "", error: null });
    fetch(url)
      .then((r) => {
        if (!r.ok) throw new Error(`${r.status} ${r.statusText}`);
        return r.text();
      })
      .then((t) => live && setState({ loading: false, html: renderMarkdown(t), error: null }))
      .catch((e) => live && setState({ loading: false, html: "", error: String(e.message || e) }));
    return () => {
      live = false;
    };
  }, [url, text]);

  if (state.loading) return html`<div class="md-status">lädt …</div>`;
  if (state.error)
    return html`<div class="md-status md-status--error">
      <strong>Konnte nicht laden.</strong><br />${state.error}<br />
      <span class="muted">Liegt das Living-Doc lokal vor? (Doc-Root evtl. nicht gefunden — siehe Server-Log.)</span>
    </div>`;
  return html`<article class="md" dangerouslySetInnerHTML=${{ __html: state.html }} />`;
}
