// System Log console: the dock's System Log tab as a live view of the
// server's real journal. An admin-gated SSE (GET /a/designer/syslog)
// delivers the server's recent-log ring as a backlog, then every new
// log line live. The toolbar follows the MQTT console's style: level
// filter, text filter, pause, clear. Every field is escaped before it
// touches innerHTML (log messages can carry device- and user-
// controlled strings).

const CAP = 2000;   // entries retained in memory
const SHOWN = 600;  // rows rendered at once

let entries = [];   // {t:unix ms, level, sys, msg}
let paused = false;
let minRank = 0;    // 0 = all levels
let filterText = '';
let es = null;
let hostEl = null;
let stylesDone = false;
let rafPending = false;

// slog level names rank by prefix so custom offsets ("INFO+2") sort
// with their base level.
const RANKS = [['ERROR', 40], ['WARN', 30], ['INFO', 20], ['DEBUG', 10]];
function rank(level) {
  const s = String(level || '').toUpperCase();
  for (const [k, r] of RANKS) if (s.startsWith(k)) return r;
  return 20;
}

function esc(s) {
  return String(s == null ? '' : s).replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}
function hhmmss(ms) { const d = new Date(ms), p = n => String(n).padStart(2, '0'); return p(d.getHours()) + ':' + p(d.getMinutes()) + ':' + p(d.getSeconds()); }

function injectStyles() {
  if (stylesDone) return; stylesDone = true;
  const css = `
  .slc{flex:1 1 0;min-width:0;display:flex;flex-direction:column;height:100%;min-height:0;font:12px/1.5 var(--mono,monospace)}
  .slc-bar{display:flex;flex-wrap:wrap;align-items:center;gap:6px;padding:6px 8px;border-bottom:1px solid var(--border,#1d2730);background:rgba(255,255,255,.02)}
  .slc-bar input,.slc-bar select{background:#0c1116;border:1px solid var(--border,#1d2730);color:var(--text,#cfe);border-radius:5px;padding:4px 7px;font:12px var(--mono,monospace);min-width:0}
  .slc-bar input:focus,.slc-bar select:focus{outline:1px solid var(--accent,#34e4ea)}
  .slc-btn{background:#101820;border:1px solid var(--border,#1d2730);color:var(--text,#cfe);border-radius:5px;padding:4px 9px;cursor:pointer;font:12px var(--mono,monospace);white-space:nowrap}
  .slc-btn:hover{background:#16212b}
  .slc-btn.on{background:color-mix(in srgb,var(--accent,#34e4ea) 18%,transparent);color:var(--accent,#34e4ea)}
  .slc-dot{width:8px;height:8px;border-radius:50%;background:#5a6b78;flex:none}
  .slc-dot.up{background:#43e08a;box-shadow:0 0 7px #43e08a}
  .slc-dot.err{background:#ff6b6b;box-shadow:0 0 7px #ff6b6b}
  .slc-sp{flex:1 1 auto}
  .slc-sub{font-size:10.5px;color:#7b848f}
  .slc-body{flex:1 1 auto;min-height:0;overflow:auto;padding:4px 8px}
  .slc-row{display:flex;gap:8px;align-items:baseline;padding:1px 0;border-bottom:1px solid rgba(255,255,255,.03)}
  .slc-row .t{color:#5a6b78;flex:none}
  .slc-row .lv{flex:none;width:5ch;font-weight:600}
  .slc-row .lv.info{color:#43e08a}
  .slc-row .lv.warn{color:#f6b23c}
  .slc-row .lv.err{color:#ff6b6b}
  .slc-row .lv.dbg{color:#7b848f}
  .slc-row .sy{color:var(--accent,#34e4ea);flex:none;max-width:16ch;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
  .slc-row .m{color:#cdd9e1;word-break:break-word;flex:1 1 auto}
  .slc-empty{color:#7b848f;padding:8px 2px}`;
  const el = document.createElement('style'); el.textContent = css; document.head.appendChild(el);
}

function setStatus(state) {
  if (!hostEl) return;
  const dot = hostEl.querySelector('.slc-dot'), lbl = hostEl.querySelector('[data-status]');
  if (dot) dot.className = 'slc-dot' + (state === 'up' ? ' up' : state === 'err' ? ' err' : '');
  if (lbl) lbl.textContent = state === 'up' ? 'live' : state === 'err' ? 'getrennt — verbinde neu…' : 'verbinde…';
}

function rowHTML(e) {
  const r = rank(e.level), cls = r >= 40 ? 'err' : r >= 30 ? 'warn' : r <= 10 ? 'dbg' : 'info';
  return `<div class="slc-row"><span class="t">${hhmmss(e.t)}</span>`
    + `<span class="lv ${cls}">${esc(e.level)}</span>`
    + `<span class="sy" title="${esc(e.sys)}">${esc(e.sys || '—')}</span>`
    + `<span class="m">${esc(e.msg)}</span></div>`;
}

function scheduleRender() {
  if (rafPending) return; rafPending = true;
  requestAnimationFrame(() => { rafPending = false; renderList(); });
}

function renderList() {
  if (!hostEl) return;
  const body = hostEl.querySelector('.slc-body'); if (!body) return;
  const q = filterText.toLowerCase();
  const list = entries.filter(e =>
    (!minRank || rank(e.level) >= minRank) &&
    (!q || (e.msg || '').toLowerCase().includes(q) || (e.sys || '').toLowerCase().includes(q) || (e.level || '').toLowerCase().includes(q)));
  const stick = body.scrollTop + body.clientHeight >= body.scrollHeight - 28;
  const slice = list.slice(-SHOWN);
  body.innerHTML = slice.length ? slice.map(rowHTML).join('') : '<div class="slc-empty">— keine Log-Einträge —</div>';
  if (stick) body.scrollTop = body.scrollHeight;
}

// startSysLog opens the SSE once (dock calls it when the tab first
// activates). The backlog event replaces the list wholesale, so an
// EventSource auto-reconnect resyncs cleanly instead of duplicating.
export function startSysLog() {
  if (es) return;
  try { es = new EventSource('syslog'); } catch (_) { setStatus('err'); return; }
  es.addEventListener('backlog', ev => {
    try { entries = (JSON.parse(ev.data).entries || []).slice(-CAP); } catch (_) { entries = []; }
    scheduleRender();
  });
  es.addEventListener('entry', ev => {
    let e; try { e = JSON.parse(ev.data); } catch (_) { return; }
    entries.push(e);
    if (entries.length > CAP) entries = entries.slice(-CAP);
    if (!paused) scheduleRender();
  });
  es.onopen = () => setStatus('up');
  es.onerror = () => setStatus('err');
}

// mountSysLog takes over the System Log dock pane with the console UI.
// Module state (entries, stream) survives a re-mount.
export function mountSysLog(host) {
  hostEl = host;
  injectStyles();
  host.innerHTML = `
    <div class="slc">
      <div class="slc-bar">
        <span class="slc-dot"></span><span class="slc-sub" data-status>verbinde…</span>
        <span class="slc-sp"></span>
        <select data-level title="Level-Filter">
          <option value="0">alle Level</option>
          <option value="20">ab INFO</option>
          <option value="30">ab WARN</option>
          <option value="40">nur ERROR</option>
        </select>
        <input data-filter type="search" placeholder="filtern…" size="14" title="Text-Filter (Nachricht, Subsystem, Level)">
        <button class="slc-btn" data-pause type="button" title="Live-Strom einfrieren">Pause</button>
        <button class="slc-btn" data-clear type="button" title="Anzeige leeren (Server-Puffer bleibt)">Leeren</button>
      </div>
      <div class="slc-body"><div class="slc-empty">— keine Log-Einträge —</div></div>
    </div>`;

  const lvl = host.querySelector('[data-level]');
  lvl.value = String(minRank);
  lvl.onchange = e => { minRank = parseInt(e.target.value, 10) || 0; renderList(); };
  const flt = host.querySelector('[data-filter]');
  flt.value = filterText;
  flt.oninput = e => { filterText = e.target.value; renderList(); };
  host.querySelector('[data-clear]').onclick = () => { entries = []; renderList(); };
  const pauseBtn = host.querySelector('[data-pause]');
  const paintPause = () => { pauseBtn.classList.toggle('on', paused); pauseBtn.textContent = paused ? 'Weiter' : 'Pause'; };
  pauseBtn.onclick = () => { paused = !paused; paintPause(); if (!paused) renderList(); };
  paintPause();

  if (es) setStatus(es.readyState === 1 ? 'up' : 'err');
  if (entries.length) renderList();
}
