// MQTT console: a full MQTT-Explorer-style client living in the dock's
// MQTT tab. Connect to our broker over WebSocket with a device login,
// subscribe to `#` to see ALL traffic, publish, filter, sort, pause and
// clear. The transport is the first-party mqtt-ws.js client (no external
// library); the broker URL + scheme come from /a/mqtt/ws-info.
//
// Everything device-controlled (topics, payloads) is escaped before it
// touches innerHTML.

import { mqttConnect } from '../vendor/mqtt-ws.js';

const CAP = 2000;        // messages retained in memory
const SHOWN = 600;       // rows rendered at once

let client = null;
let connected = false;
let paused = false;
let sortMode = 'time';   // 'time' | 'topic'
let filterText = '';
let messages = [];       // {t:Date, topic, payload:string, qos, retain}
let rafPending = false;
let stylesDone = false;

function esc(s) {
  return String(s == null ? '' : s).replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}
function hhmmss(d) { const p = n => String(n).padStart(2, '0'); return p(d.getHours()) + ':' + p(d.getMinutes()) + ':' + p(d.getSeconds()); }

function injectStyles() {
  if (stylesDone) return; stylesDone = true;
  const css = `
  .mqc{flex:1 1 0;min-width:0;display:flex;flex-direction:column;height:100%;min-height:0;font:12px/1.5 var(--mono,monospace)}
  .mqc-bar{display:flex;flex-wrap:wrap;align-items:center;gap:6px;padding:6px 8px;border-bottom:1px solid var(--border,#1d2730);background:rgba(255,255,255,.02)}
  .mqc-bar input,.mqc-bar select{background:#0c1116;border:1px solid var(--border,#1d2730);color:var(--text,#cfe);border-radius:5px;padding:4px 7px;font:12px var(--mono,monospace);min-width:0}
  .mqc-bar input:focus,.mqc-bar select:focus{outline:1px solid var(--accent,#34e4ea)}
  .mqc-btn{background:#101820;border:1px solid var(--border,#1d2730);color:var(--text,#cfe);border-radius:5px;padding:4px 9px;cursor:pointer;font:12px var(--mono,monospace);white-space:nowrap}
  .mqc-btn:hover{background:#16212b}
  .mqc-btn.go{border-color:color-mix(in srgb,var(--accent,#34e4ea) 60%,transparent);color:var(--accent,#34e4ea)}
  .mqc-btn.on{background:color-mix(in srgb,var(--accent,#34e4ea) 18%,transparent);color:var(--accent,#34e4ea)}
  .mqc-dot{width:8px;height:8px;border-radius:50%;background:#5a6b78;flex:none}
  .mqc-dot.up{background:#43e08a;box-shadow:0 0 7px #43e08a}
  .mqc-dot.err{background:#ff6b6b;box-shadow:0 0 7px #ff6b6b}
  .mqc-sp{flex:1 1 auto}
  .mqc-div{width:1px;height:20px;background:var(--border,#1d2730);flex:none;margin:0 3px}
  .mqc-sub{font-size:10.5px;color:#7b848f}
  .mqc-body{flex:1 1 auto;min-height:0;overflow:auto;padding:4px 8px}
  .mqc-row{display:flex;gap:8px;align-items:baseline;padding:1px 0;border-bottom:1px solid rgba(255,255,255,.03)}
  .mqc-row .t{color:#5a6b78;flex:none}
  .mqc-row .tp{color:var(--accent,#34e4ea);flex:none;max-width:42%;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
  .mqc-row .pl{color:#cdd9e1;word-break:break-word;flex:1 1 auto}
  .mqc-row .meta{color:#7b848f;flex:none;font-size:10.5px}
  .mqc-row .rt{color:#f6b23c}
  .mqc-empty{color:#7b848f;padding:8px 2px}
  .mqc-login{display:flex;gap:6px;align-items:center}
  .mqc-login.hide{display:none}`;
  const el = document.createElement('style'); el.textContent = css; document.head.appendChild(el);
}

function setStatus(host, state) {
  const dot = host.querySelector('.mqc-dot'), lbl = host.querySelector('[data-status]');
  if (dot) dot.className = 'mqc-dot' + (state === 'up' ? ' up' : state === 'err' ? ' err' : '');
  if (lbl) lbl.textContent = state === 'up' ? 'verbunden' : state === 'err' ? 'Fehler' : 'getrennt';
  const cbtn = host.querySelector('[data-connect]');
  if (cbtn) cbtn.textContent = state === 'up' ? 'Trennen' : 'Verbinden';
}

function rowHTML(m) {
  return `<div class="mqc-row"><span class="t">${hhmmss(m.t)}</span>`
    + `<span class="tp" title="${esc(m.topic)}">${esc(m.topic)}</span>`
    + `<span class="pl">${esc(m.payload)}</span>`
    + `<span class="meta">q${m.qos}${m.retain ? ' <span class="rt">R</span>' : ''}</span></div>`;
}

function scheduleRender(host) {
  if (rafPending) return; rafPending = true;
  requestAnimationFrame(() => { rafPending = false; renderList(host); });
}

function renderList(host) {
  const body = host.querySelector('.mqc-body'); if (!body) return;
  const q = filterText.toLowerCase();
  let list = q ? messages.filter(m => m.topic.toLowerCase().includes(q) || m.payload.toLowerCase().includes(q)) : messages.slice();
  if (sortMode === 'topic') list.sort((a, b) => a.topic < b.topic ? -1 : a.topic > b.topic ? 1 : a.t - b.t);
  const stick = sortMode === 'time' && body.scrollTop + body.clientHeight >= body.scrollHeight - 28;
  const slice = sortMode === 'time' ? list.slice(-SHOWN) : list.slice(0, SHOWN);
  body.innerHTML = slice.length ? slice.map(rowHTML).join('') : '<div class="mqc-empty">— keine Nachrichten —</div>';
  if (stick) body.scrollTop = body.scrollHeight;
}

function onMessage(host, topic, payloadBytes, meta) {
  const payload = new TextDecoder().decode(payloadBytes || new Uint8Array(0));
  messages.push({ t: new Date(), topic, payload, qos: meta.qos, retain: meta.retain });
  if (messages.length > CAP) messages = messages.slice(-CAP);
  if (!paused) scheduleRender(host);
}

function doConnect(host) {
  if (connected) { try { client && client.end(); } catch (_) {} client = null; connected = false; setStatus(host, 'down'); return; }
  const url = host.querySelector('[data-url]').value.trim();
  const user = host.querySelector('[data-user]').value;
  const pass = host.querySelector('[data-pass]').value;
  if (!url) return;
  setStatus(host, 'down');
  try {
    client = mqttConnect(url, { username: user || undefined, password: pass || undefined });
  } catch (e) { setStatus(host, 'err'); return; }
  client.on('connect', () => { connected = true; setStatus(host, 'up'); client.subscribe('#', 0); });
  client.on('message', (t, p, m) => onMessage(host, t, p, m));
  client.on('close', () => { connected = false; setStatus(host, 'down'); });
  client.on('error', () => { connected = false; setStatus(host, 'err'); });
}

function doPublish(host) {
  if (!connected || !client) return;
  const topic = host.querySelector('[data-ptopic]').value.trim();
  if (!topic) return;
  const msg = host.querySelector('[data-pmsg]').value;
  const qos = parseInt(host.querySelector('[data-pqos]').value, 10) || 0;
  const retain = host.querySelector('[data-pretain]').checked;
  client.publish(topic, msg, { qos, retain });
}

function prefillUrl(host) {
  fetch('/a/mqtt/ws-info', { credentials: 'same-origin' }).then(r => r.ok ? r.json() : null).then(info => {
    const urlEl = host.querySelector('[data-url]'); if (!urlEl) return;
    if (info && info.enabled && info.url) urlEl.value = info.url;
    else { urlEl.placeholder = 'WebSocket-Listener aus — in den Einstellungen aktivieren'; }
  }).catch(() => {});
}

// mountMqttConsole takes over the MQTT dock pane with the client UI.
export function mountMqttConsole(host) {
  injectStyles();
  // One horizontal toolbar (was two rows): login+connect · publish · view
  // controls. flex-wrap only folds it at genuinely narrow widths. The
  // settings link is icon-only (gear) to keep the single row compact.
  host.innerHTML = `
    <div class="mqc">
      <div class="mqc-bar">
        <button class="mqc-btn" data-toggle-login type="button" title="Login ein/aus">▸ Login</button>
        <span class="mqc-login hide" data-login>
          <input data-url type="text" placeholder="ws://…" size="18" title="Broker-WebSocket-URL">
          <input data-user type="text" placeholder="Benutzer" size="9" autocomplete="off">
          <input data-pass type="password" placeholder="Passwort" size="9" autocomplete="off">
        </span>
        <button class="mqc-btn go" data-connect type="button">Verbinden</button>
        <span class="mqc-dot"></span><span class="mqc-sub" data-status>getrennt</span>
        <span class="mqc-div"></span>
        <input data-ptopic type="text" placeholder="Publish-Topic" size="16">
        <input data-pmsg type="text" placeholder="Nachricht" size="14">
        <select data-pqos title="QoS"><option value="0">QoS 0</option><option value="1">QoS 1</option></select>
        <label class="mqc-sub" style="display:flex;gap:4px;align-items:center"><input data-pretain type="checkbox">Retain</label>
        <button class="mqc-btn" data-publish type="button">Senden</button>
        <span class="mqc-sp"></span>
        <input data-filter type="search" placeholder="filtern…" size="12">
        <select data-sort title="Sortierung"><option value="time">Zeit</option><option value="topic">Topic</option></select>
        <button class="mqc-btn" data-pause type="button" title="Live-Strom einfrieren">Pause</button>
        <button class="mqc-btn" data-clear type="button">Leeren</button>
        <a class="mqc-btn" href="/a/mqtt" target="_top" title="Broker-Einstellungen" aria-label="Broker-Einstellungen">⚙</a>
      </div>
      <div class="mqc-body"><div class="mqc-empty">Nicht verbunden. Zugangsdaten unter „▸ Login" einblenden, dann „Verbinden". Tipp: Das Konsolen-Gerät braucht eine Subscribe-Regel auf <b>#</b> (in den Einstellungen), um allen Verkehr zu sehen.</div></div>
    </div>`;

  host.querySelector('[data-connect]').onclick = () => doConnect(host);
  host.querySelector('[data-publish]').onclick = () => doPublish(host);
  // Login starts collapsed so the steady-state toolbar (watching traffic)
  // stays a single row; expanding it to connect may wrap at narrow widths.
  const loginBtn = host.querySelector('[data-toggle-login]');
  loginBtn.onclick = () => { const hidden = host.querySelector('[data-login]').classList.toggle('hide'); loginBtn.textContent = (hidden ? '▸' : '▾') + ' Login'; };
  host.querySelector('[data-filter]').oninput = e => { filterText = e.target.value; renderList(host); };
  host.querySelector('[data-sort]').onchange = e => { sortMode = e.target.value; renderList(host); };
  host.querySelector('[data-clear]').onclick = () => { messages = []; renderList(host); };
  const pauseBtn = host.querySelector('[data-pause]');
  pauseBtn.onclick = () => { paused = !paused; pauseBtn.classList.toggle('on', paused); pauseBtn.textContent = paused ? 'Weiter' : 'Pause'; if (!paused) renderList(host); };
  // Enter in the publish message sends; in the login fields connects.
  host.querySelector('[data-pmsg]').addEventListener('keydown', e => { if (e.key === 'Enter') doPublish(host); });
  host.querySelector('[data-pass]').addEventListener('keydown', e => { if (e.key === 'Enter') doConnect(host); });

  setStatus(host, connected ? 'up' : 'down');
  prefillUrl(host);
  if (messages.length) renderList(host);
}
