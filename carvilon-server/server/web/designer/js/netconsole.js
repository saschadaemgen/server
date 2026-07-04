// TCP/UDP line consoles (Terminal-Track step 2): the dock's TCP and UDP
// tabs, real end to end. The browser cannot open raw sockets, so each
// pane talks WebSocket to the Go bridge (console/ws, same session frame
// as the SSH terminal): hello {kind:"tcp"|"udp",...}, then binary frames
// are payload bytes and text frames are status JSON.
//
// TCP is a raw byte stream: every received chunk is one log line. UDP
// has datagram boundaries and senders, so its backend wraps each event
// in one JSON stream line ({"from","b64"} / {"sys",...}) that this
// module decodes. Sending: one input line + chosen line ending = one
// binary frame = (for UDP) exactly one datagram.
//
// Line consoles in the MQTT-console style — no xterm. All byte content
// is escaped before it touches innerHTML; the ASCII view shows control
// bytes as \xNN escapes, the hex view shows the raw bytes.

const CAP = 2000;   // lines retained in memory
const SHOWN = 600;  // lines rendered at once

let stylesDone = false;
function injectStyles() {
  if (stylesDone) return; stylesDone = true;
  const css = `
  .ncx{display:flex;flex-direction:column;height:100%;min-height:0;font:12px/1.5 var(--mono,monospace)}
  .ncx-bar{display:flex;flex-wrap:wrap;align-items:center;gap:6px;padding:6px 8px;border-bottom:1px solid var(--border,#1d2730);background:rgba(255,255,255,.02)}
  .ncx-bar input,.ncx-bar select{background:#0c1116;border:1px solid var(--border,#1d2730);color:var(--text,#cfe);border-radius:5px;padding:4px 7px;font:12px var(--mono,monospace);min-width:0}
  .ncx-bar input:focus,.ncx-bar select:focus{outline:1px solid var(--accent,#34e4ea)}
  .ncx-bar label{font-size:10.5px;color:#7b848f;white-space:nowrap}
  .ncx-btn{background:#101820;border:1px solid var(--border,#1d2730);color:var(--text,#cfe);border-radius:5px;padding:4px 9px;cursor:pointer;font:12px var(--mono,monospace);white-space:nowrap}
  .ncx-btn:hover{background:#16212b}
  .ncx-btn:disabled{opacity:.4;cursor:default}
  .ncx-btn.go{border-color:color-mix(in srgb,var(--accent,#34e4ea) 60%,transparent);color:var(--accent,#34e4ea)}
  .ncx-btn.on{background:color-mix(in srgb,var(--accent,#34e4ea) 18%,transparent);color:var(--accent,#34e4ea)}
  .ncx-dot{width:8px;height:8px;border-radius:50%;background:#5a6b78;flex:none}
  .ncx-dot.up{background:#43e08a;box-shadow:0 0 7px #43e08a}
  .ncx-dot.err{background:#ff6b6b;box-shadow:0 0 7px #ff6b6b}
  .ncx-sub{font-size:10.5px;color:#7b848f;white-space:nowrap}
  .ncx-sp{flex:1 1 auto}
  .ncx-body{flex:1 1 auto;min-height:0;overflow:auto;padding:4px 8px}
  .ncx-row{display:flex;gap:8px;align-items:baseline;padding:1px 0;border-bottom:1px solid rgba(255,255,255,.03)}
  .ncx-row .t{color:#5a6b78;flex:none}
  .ncx-row .dir{flex:none;font-weight:600}
  .ncx-row .dir.in{color:#43e08a}
  .ncx-row .dir.out{color:var(--accent,#34e4ea)}
  .ncx-row .dir.sys{color:#f6b23c}
  .ncx-row .from{color:#8fb6c9;flex:none}
  .ncx-row .pl{color:#cdd9e1;word-break:break-all;white-space:pre-wrap;flex:1 1 auto}
  .ncx-row .pl.hex{color:#a9c1d0;letter-spacing:.04em}
  .ncx-row .pl .esc{color:#f6b23c}
  .ncx-row .meta{color:#7b848f;flex:none;font-size:10.5px}
  .ncx-empty{color:#7b848f;padding:8px 2px}`;
  const el = document.createElement('style'); el.textContent = css; document.head.appendChild(el);
}

function esc(s) {
  return String(s == null ? '' : s).replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}
function hhmmss(d) { const p = n => String(n).padStart(2, '0'); return p(d.getHours()) + ':' + p(d.getMinutes()) + ':' + p(d.getSeconds()); }

// asciiHTML renders bytes as escaped text: printable UTF-8 stays, control
// bytes become highlighted \xNN / \r \n \t escapes (a log line must never
// wrap or vanish because the device sent control codes).
function asciiHTML(bytes) {
  const txt = new TextDecoder('utf-8', { fatal: false }).decode(bytes);
  let out = '';
  for (const ch of txt) {
    const c = ch.codePointAt(0);
    if (ch === '\n') out += '<span class="esc">\\n</span>';
    else if (ch === '\r') out += '<span class="esc">\\r</span>';
    else if (ch === '\t') out += '<span class="esc">\\t</span>';
    else if (c < 0x20 || c === 0x7f) out += '<span class="esc">\\x' + c.toString(16).padStart(2, '0') + '</span>';
    else out += esc(ch);
  }
  return out;
}
// asciiPlain is the same rendering without markup — the filter matches it.
function asciiPlain(bytes) {
  const txt = new TextDecoder('utf-8', { fatal: false }).decode(bytes);
  let out = '';
  for (const ch of txt) {
    const c = ch.codePointAt(0);
    if (ch === '\n') out += '\\n';
    else if (ch === '\r') out += '\\r';
    else if (ch === '\t') out += '\\t';
    else if (c < 0x20 || c === 0x7f) out += '\\x' + c.toString(16).padStart(2, '0');
    else out += ch;
  }
  return out;
}
function hexPlain(bytes) {
  let out = '';
  for (let i = 0; i < bytes.length; i++) out += (i ? ' ' : '') + bytes[i].toString(16).padStart(2, '0');
  return out;
}

function wsURL() {
  const u = new URL('console/ws', location.href);
  u.protocol = u.protocol === 'https:' ? 'wss:' : 'ws:';
  return u.toString();
}

const EOL_BYTES = { lf: '\n', crlf: '\r\n', cr: '\r', none: '' };

// mountNetConsole takes over one dock pane with a TCP or UDP line
// console. Single-instance per tab; all state lives in this closure so
// the two tabs never share anything.
export function mountNetConsole(host, proto) {
  injectStyles();
  const isUDP = proto === 'udp';
  const enc = new TextEncoder();

  let ws = null;
  let connected = false;
  let paused = false;
  let hexView = false;
  let filterText = '';
  let lines = [];         // {t, dir:'in'|'out'|'sys', from, bytes|null, text|null}
  let rafPending = false;
  let udpBuf = '';        // partial JSON stream line from the UDP backend
  let sessionTarget = true; // does the LIVE session have a send target? (UDP can run listen-only)

  const connFields = isUDP
    ? `<label>Lauschen</label><input data-listen type="number" min="1" max="65535" placeholder="Port" style="width:70px" title="Lokaler UDP-Lausch-Port (leer = nur senden)">
       <label>Ziel</label><input data-host type="text" placeholder="host / 10.0.0.9" size="14" title="Ziel-Host (leer = nur lauschen)">
       <input data-port type="number" min="1" max="65535" placeholder="Port" style="width:70px" title="Ziel-Port">`
    : `<label>Host</label><input data-host type="text" placeholder="host / 10.0.0.9" size="14">
       <label>Port</label><input data-port type="number" min="1" max="65535" placeholder="Port" style="width:70px">`;

  host.innerHTML = `
    <div class="ncx">
      <div class="ncx-bar">
        <span class="ncx-dot"></span><span class="ncx-sub" data-status>getrennt</span>
        ${connFields}
        <button class="ncx-btn go" data-connect type="button">${isUDP ? 'Starten' : 'Verbinden'}</button>
        <span class="ncx-sp"></span>
        <span class="ncx-sub">${isUDP ? 'UDP-Datagramme · Ziele nur im LAN' : 'Roher TCP-Socket · Ziele nur im LAN'}</span>
      </div>
      <div class="ncx-bar">
        <input data-send type="text" placeholder="Senden…" size="22" ${isUDP ? 'title="Eine Eingabe = ein Datagramm ans Ziel"' : ''}>
        <select data-eol title="Zeilenende beim Senden">
          <option value="lf">LF</option><option value="crlf">CRLF</option>
          <option value="cr">CR</option><option value="none">keins</option>
        </select>
        <button class="ncx-btn" data-sendbtn type="button">Senden</button>
        <span class="ncx-sp"></span>
        <button class="ncx-btn" data-view type="button" title="Anzeige umschalten">ASCII</button>
        <input data-filter type="search" placeholder="filtern…" size="12">
        <button class="ncx-btn" data-pause type="button" title="Anzeige einfrieren">Pause</button>
        <button class="ncx-btn" data-clear type="button">Leeren</button>
      </div>
      <div class="ncx-body"><div class="ncx-empty">${isUDP
        ? 'Nicht gestartet. Lausch-Port und/oder Ziel setzen, dann „Starten". Eingehende Datagramme erscheinen mit Absender.'
        : 'Nicht verbunden. Host und Port eines Geräts im LAN eintragen, dann „Verbinden".'}</div></div>
    </div>`;

  const $ = sel => host.querySelector(sel);
  const el = {
    dot: $('.ncx-dot'), status: $('[data-status]'),
    hostIn: $('[data-host]'), portIn: $('[data-port]'), listenIn: $('[data-listen]'),
    connect: $('[data-connect]'), send: $('[data-send]'), eol: $('[data-eol]'),
    sendBtn: $('[data-sendbtn]'), view: $('[data-view]'), filter: $('[data-filter]'),
    pause: $('[data-pause]'), clear: $('[data-clear]'), body: $('.ncx-body'),
  };

  function setStatus(state, text) {
    el.dot.className = 'ncx-dot' + (state === 'up' ? ' up' : state === 'err' ? ' err' : '');
    el.status.textContent = text;
    el.connect.textContent = connected ? (isUDP ? 'Stoppen' : 'Trennen') : (isUDP ? 'Starten' : 'Verbinden');
    syncSendEnabled();
  }

  // Send is gated on the RUNNING session's target, not the live input
  // fields: a UDP session opened listen-only has no target for its whole
  // life even if the user later types into the Ziel field (the backend
  // would drop those datagrams). While disconnected the button is off
  // regardless.
  function syncSendEnabled() {
    const noTarget = isUDP && connected && !sessionTarget;
    el.sendBtn.disabled = !connected || noTarget;
    el.sendBtn.title = noTarget ? 'Kein Ziel gesetzt — nur Lauschen' : '';
  }

  function pushLine(l) {
    l.t = new Date();
    lines.push(l);
    if (lines.length > CAP) lines = lines.slice(-CAP);
    if (!paused) scheduleRender();
  }
  function pushSys(text) { pushLine({ dir: 'sys', from: null, bytes: null, text }); }

  function lineMatches(l, q) {
    if (l.text && l.text.toLowerCase().includes(q)) return true;
    if (l.from && l.from.toLowerCase().includes(q)) return true;
    if (l.bytes && asciiPlain(l.bytes).toLowerCase().includes(q)) return true;
    return false;
  }

  function rowHTML(l) {
    const dir = l.dir === 'in' ? '◂' : l.dir === 'out' ? '▸' : '·';
    let pl, meta = '';
    if (l.bytes) {
      pl = hexView ? `<span class="pl hex">${hexPlain(l.bytes)}</span>` : `<span class="pl">${asciiHTML(l.bytes)}</span>`;
      meta = `<span class="meta">${l.bytes.length} B</span>`;
    } else {
      pl = `<span class="pl">${esc(l.text)}</span>`;
    }
    const from = l.from ? `<span class="from">${esc(l.from)}</span>` : '';
    return `<div class="ncx-row"><span class="t">${hhmmss(l.t)}</span>`
      + `<span class="dir ${l.dir}">${dir}</span>${from}${pl}${meta}</div>`;
  }

  function scheduleRender() {
    if (rafPending) return; rafPending = true;
    requestAnimationFrame(() => { rafPending = false; renderList(); });
  }
  function renderList() {
    const q = filterText.toLowerCase();
    const list = q ? lines.filter(l => lineMatches(l, q)) : lines;
    const stick = el.body.scrollTop + el.body.clientHeight >= el.body.scrollHeight - 28;
    const slice = list.slice(-SHOWN);
    el.body.innerHTML = slice.length ? slice.map(rowHTML).join('') : '<div class="ncx-empty">— keine Daten —</div>';
    if (stick) el.body.scrollTop = el.body.scrollHeight;
  }

  // ---- the UDP backend's JSON stream lines ----
  function onUDPStream(bytes) {
    udpBuf += new TextDecoder().decode(bytes);
    let nl;
    while ((nl = udpBuf.indexOf('\n')) >= 0) {
      const raw = udpBuf.slice(0, nl); udpBuf = udpBuf.slice(nl + 1);
      let m; try { m = JSON.parse(raw); } catch (_) { continue; }
      if (m.sys === 'listening') { pushSys('lauscht auf Port ' + m.port); continue; }
      if (m.sys) { pushSys(m.sys); continue; }
      // A received datagram is keyed on `from`; its payload (m.b64) may be
      // "" for a legal empty datagram — still a visible event.
      if (m.from != null) {
        let arr;
        try { const bin = atob(m.b64 || ''); arr = new Uint8Array(bin.length); for (let i = 0; i < bin.length; i++) arr[i] = bin.charCodeAt(i); }
        catch (_) { arr = new Uint8Array(0); }
        pushLine({ dir: 'in', from: m.from, bytes: arr, text: null });
      }
    }
  }

  // ---- connect / disconnect ----
  // disconnect resets the pane state itself: it nulls ws first, so the
  // socket's own onclose (guarded by ws===sock) stays a no-op — that
  // path is for the SERVER side going away, this one for the user.
  function disconnect() {
    const sock = ws;
    ws = null;
    if (sock) { try { sock.close(); } catch (_) { /* already closed */ } }
    if (connected) { connected = false; pushSys('getrennt'); }
    setStatus('', 'getrennt');
  }

  function doConnect() {
    if (connected || ws) { disconnect(); return; }
    const hello = { kind: proto };
    const h = el.hostIn.value.trim();
    const p = parseInt(el.portIn.value, 10) || 0;
    if (isUDP) {
      const lp = parseInt(el.listenIn.value, 10) || 0;
      if (!lp && !h) { setStatus('err', 'Lausch-Port oder Ziel fehlt'); return; }
      if (h && !p) { setStatus('err', 'Ziel-Port fehlt'); return; }
      hello.listen_port = lp;
      if (h) { hello.host = h; hello.port = p; }
      sessionTarget = !!h; // fixed for this session's whole life
    } else {
      if (!h || !p) { setStatus('err', 'Host/Port fehlt'); return; }
      hello.host = h; hello.port = p;
      sessionTarget = true;
    }
    setStatus('', 'verbinde…');
    udpBuf = '';
    const sock = new WebSocket(wsURL());
    sock.binaryType = 'arraybuffer';
    ws = sock;
    sock.onopen = () => sock.send(JSON.stringify(hello));
    sock.onmessage = ev => {
      if (typeof ev.data === 'string') { handleStatus(ev.data); return; }
      const bytes = new Uint8Array(ev.data);
      if (isUDP) onUDPStream(bytes);
      else pushLine({ dir: 'in', from: null, bytes, text: null });
    };
    sock.onclose = () => {
      if (ws === sock) {
        ws = null;
        if (connected) { connected = false; pushSys('getrennt'); }
        if (el.dot.className.indexOf('err') < 0) setStatus('', 'getrennt');
      }
    };
    sock.onerror = () => { /* onclose follows */ };
  }

  function handleStatus(raw) {
    let m; try { m = JSON.parse(raw); } catch (_) { return; }
    if (m.t !== 'status') return;
    if (m.state === 'connected') {
      connected = true;
      const h = el.hostIn.value.trim(), p = el.portIn.value;
      setStatus('up', isUDP ? 'aktiv' : h + ':' + p);
      pushSys(isUDP ? 'gestartet' : 'verbunden mit ' + h + ':' + p);
      el.send.focus();
    } else if (m.state === 'error') {
      connected = false;
      const detail = String(m.detail || 'Verbindung fehlgeschlagen');
      setStatus('err', 'Fehler');
      pushSys('Fehler: ' + detail);
    } else if (m.state === 'closed') {
      connected = false;
    }
  }

  function doSend() {
    if (!connected || !ws || ws.readyState !== 1 || el.sendBtn.disabled) return;
    const text = el.send.value + (EOL_BYTES[el.eol.value] || '');
    if (!text.length) return;
    const bytes = enc.encode(text);
    ws.send(bytes);
    pushLine({ dir: 'out', from: null, bytes, text: null });
    el.send.value = '';
    el.send.focus();
  }

  // ---- wiring ----
  el.connect.onclick = doConnect;
  el.sendBtn.onclick = doSend;
  el.send.addEventListener('keydown', e => { if (e.key === 'Enter') doSend(); });
  el.portIn.addEventListener('keydown', e => { if (e.key === 'Enter') doConnect(); });
  if (el.listenIn) el.listenIn.addEventListener('keydown', e => { if (e.key === 'Enter') doConnect(); });
  el.view.onclick = () => {
    hexView = !hexView;
    el.view.textContent = hexView ? 'Hex' : 'ASCII';
    el.view.classList.toggle('on', hexView);
    renderList();
  };
  el.filter.oninput = e => { filterText = e.target.value; renderList(); };
  el.pause.onclick = () => {
    paused = !paused;
    el.pause.classList.toggle('on', paused);
    el.pause.textContent = paused ? 'Weiter' : 'Pause';
    if (!paused) renderList();
  };
  el.clear.onclick = () => { lines = []; renderList(); };

  setStatus('', 'getrennt');
  syncSendEnabled();
}
