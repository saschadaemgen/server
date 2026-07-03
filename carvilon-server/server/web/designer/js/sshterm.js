// SSH terminal pane (Terminal-Track step 1). Each SSH dock column is an
// independent terminal: its own xterm.js emulator, its own WebSocket to
// /a/designer/console/ws, and its own toolbar. The browser can't speak
// SSH, so the Go server bridges pane <-> WebSocket <-> target (a local
// shell PTY on the edge host, or an outbound SSH client).
//
// Wire protocol (mirrors handler_admin_console.go): the first frame is a
// text "hello" JSON selecting the backend; then binary frames are raw
// keystrokes and text frames are control JSON ({"t":"resize",...}). From
// the server, binary frames are terminal output and text frames are
// status/error control lines. Ad-hoc credentials travel inside the hello
// frame (never the URL), so they stay out of access logs.
//
// xterm.js is vendored locally (vendor/xterm/, MIT) and imported on
// demand — no CDN, no external request.

let xtermMod = null;   // lazily imported { Terminal }
let fitMod = null;     // lazily imported { FitAddon }
let cssInjected = false;
let capsCache = null;  // { local_shell, profiles }
let profilesCache = null;

function esc(s) {
  return String(s == null ? '' : s).replace(/[&<>"']/g, c =>
    ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}

// injectStyles adds the pane's CSS once (same pattern as syslog.js /
// mqttconsole.js). The xterm base stylesheet is linked separately.
function injectStyles() {
  if (cssInjected) return;
  cssInjected = true;
  if (!document.querySelector('link[data-xterm]')) {
    const link = document.createElement('link');
    link.rel = 'stylesheet';
    link.href = 'vendor/xterm/xterm.css';
    link.dataset.xterm = '1';
    document.head.appendChild(link);
  }
  const css = `
  .ssh-pane{display:flex;flex-direction:column;height:100%;min-height:0;background:#0a0e12}
  .ssh-bar{display:flex;flex-wrap:wrap;align-items:center;gap:6px;flex:none;padding:5px 8px;border-bottom:1px solid rgba(255,255,255,.06);background:rgba(120,150,180,.05)}
  .ssh-dot{width:8px;height:8px;border-radius:50%;background:#5a6b78;flex:none}
  .ssh-dot.up{background:#22C55E;box-shadow:0 0 7px #22C55E}
  .ssh-dot.busy{background:#f6b23c;box-shadow:0 0 7px #f6b23c}
  .ssh-dot.err{background:#ff6b6b;box-shadow:0 0 7px #ff6b6b}
  .ssh-stat{font:600 10.5px/1 var(--mono,monospace);color:var(--muted,#8A97A6);white-space:nowrap;max-width:18ch;overflow:hidden;text-overflow:ellipsis}
  .ssh-sp{flex:1 1 auto}
  .ssh-bar select,.ssh-bar input,.ssh-connect input,.ssh-connect select,.ssh-connect textarea{background:rgba(0,0,0,.32);border:1px solid var(--border,#26333f);color:var(--text,#EAF1F5);border-radius:6px;padding:4px 7px;font:500 11.5px var(--mono,monospace);min-width:0}
  .ssh-bar select:focus,.ssh-bar input:focus,.ssh-connect input:focus,.ssh-connect textarea:focus{outline:none;border-color:var(--accent,#3B82F6)}
  .ssh-btn{background:rgba(120,150,180,.09);border:1px solid var(--border,#26333f);color:var(--text,#EAF1F5);border-radius:6px;padding:4px 9px;cursor:pointer;font:600 11px var(--mono,monospace);white-space:nowrap;display:inline-flex;align-items:center;gap:5px}
  .ssh-btn:hover{background:rgba(120,150,180,.16);border-color:var(--border-bright,#4a5a68)}
  .ssh-btn.primary{background:color-mix(in srgb,var(--accent,#3B82F6) 22%,transparent);border-color:var(--accent,#3B82F6);color:#dce9ff}
  .ssh-btn.danger:hover{color:#ff8a8a;border-color:#ff6b6b}
  .ssh-btn:disabled{opacity:.4;cursor:default}
  .ssh-x{flex:none;width:20px;height:20px;padding:0;border:1px solid transparent;border-radius:5px;background:transparent;color:var(--faint,#52606E);font:600 14px/1 var(--font,sans-serif);cursor:pointer;display:grid;place-items:center}
  .ssh-host-solo .ssh-x{display:none}
  .ssh-x:hover{color:#ff7a7a;border-color:#ff7a7a;background:rgba(255,90,90,.12)}
  .ssh-body{flex:1 1 auto;min-height:0;overflow:hidden;padding:4px 6px;position:relative}
  .ssh-body .xterm{height:100%}
  .ssh-empty{position:absolute;inset:0;display:grid;place-items:center;text-align:center;color:var(--faint,#52606E);font:500 11.5px/1.6 var(--mono,monospace);pointer-events:none;padding:16px}
  .ssh-panel{flex:none;border-bottom:1px solid rgba(255,255,255,.06);background:rgba(8,12,16,.6);padding:8px 10px;display:none}
  .ssh-panel.open{display:block}
  .ssh-row{display:flex;flex-wrap:wrap;gap:6px;align-items:center;margin-bottom:6px}
  .ssh-row label{font:600 10px/1 var(--mono,monospace);letter-spacing:.04em;color:var(--muted,#8A97A6);text-transform:uppercase}
  .ssh-connect textarea{width:100%;height:70px;resize:vertical;white-space:pre}
  .ssh-connect .grow{flex:1 1 120px}
  .ssh-hkwarn{flex:none;display:none;gap:8px;align-items:center;flex-wrap:wrap;padding:8px 10px;background:rgba(255,90,90,.1);border-bottom:1px solid rgba(255,90,90,.4);color:#ffd0d0;font:500 11.5px/1.5 var(--mono,monospace)}
  .ssh-hkwarn.open{display:flex}
  .ssh-plist{list-style:none;margin:0 0 6px;padding:0;max-height:120px;overflow:auto}
  .ssh-plist li{display:flex;align-items:center;gap:8px;padding:4px 2px;border-bottom:1px solid rgba(255,255,255,.04);font:500 11.5px var(--mono,monospace);color:var(--text,#EAF1F5)}
  .ssh-plist .pmeta{flex:1 1 auto;min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
  .ssh-plist .pmeta small{color:var(--faint,#52606E)}`;
  const el = document.createElement('style');
  el.textContent = css;
  document.head.appendChild(el);
}

async function ensureXterm() {
  if (!xtermMod) {
    injectStyles();
    xtermMod = await import('../vendor/xterm/xterm.js');
    fitMod = await import('../vendor/xterm/addon-fit.js');
  }
  return xtermMod;
}

async function fetchCaps() {
  if (capsCache) return capsCache;
  try {
    const r = await fetch('console/caps', { credentials: 'same-origin' });
    capsCache = r.ok ? await r.json() : { local_shell: false, profiles: false };
  } catch (_) { capsCache = { local_shell: false, profiles: false }; }
  return capsCache;
}

async function fetchProfiles(force) {
  if (profilesCache && !force) return profilesCache;
  try {
    const r = await fetch('console/profiles', { credentials: 'same-origin' });
    const j = r.ok ? await r.json() : { profiles: [] };
    profilesCache = j.profiles || [];
  } catch (_) { profilesCache = []; }
  return profilesCache;
}

function wsURL() {
  const u = new URL('console/ws', location.href);
  u.protocol = u.protocol === 'https:' ? 'wss:' : 'ws:';
  return u.toString();
}

// mountSshPane takes over one dock column with an SSH terminal. opts:
// { closable, onClose }. Returns { dispose } so the dock can tear the
// pane's session down when its column is removed.
export function mountSshPane(host, opts = {}) {
  injectStyles();
  const enc = new TextEncoder();

  let ws = null;
  let term = null;
  let fit = null;
  let ro = null;
  let lastAttempt = null;   // remembered for reconnect / re-trust
  let pendingHostKey = null; // { host, port } when a change is flagged

  host.innerHTML = `
    <div class="ssh-pane">
      <div class="ssh-bar">
        <span class="ssh-dot"></span><span class="ssh-stat" data-stat>getrennt</span>
        <span class="ssh-sp"></span>
        <select data-prof title="Gespeichertes Profil"><option value="">— Profil —</option></select>
        <button class="ssh-btn" data-conn-prof title="Mit gewähltem Profil verbinden">Verbinden</button>
        <button class="ssh-btn" data-local title="Lokale Shell auf dem Server-Gerät" hidden>Lokale Shell</button>
        <button class="ssh-btn" data-new title="Neue SSH-Verbindung eingeben">Neu…</button>
        <button class="ssh-btn" data-manage title="Profile verwalten">Profile</button>
        <button class="ssh-btn danger" data-disc title="Trennen" disabled>Trennen</button>
        ${opts.closable ? '<button class="ssh-x" data-close title="Dieses Terminal schließen" aria-label="Terminal schließen">×</button>' : ''}
      </div>

      <div class="ssh-hkwarn" data-hkwarn>
        <span data-hkmsg></span>
        <button class="ssh-btn danger" data-hktrust>Neu vertrauen &amp; verbinden</button>
        <button class="ssh-btn" data-hkcancel>Abbrechen</button>
      </div>

      <div class="ssh-panel ssh-connect" data-connect>
        <div class="ssh-row">
          <label>Host</label><input data-host class="grow" placeholder="host.example / 10.0.0.9">
          <label>Port</label><input data-port type="number" value="22" style="width:64px">
          <label>Benutzer</label><input data-user class="grow" placeholder="root">
        </div>
        <div class="ssh-row">
          <label>Anmeldung</label>
          <label><input type="radio" name="auth-${uid()}" value="password" data-auth checked> Passwort</label>
          <label><input type="radio" name="auth-${uid()}" value="key" data-auth> Schlüssel</label>
        </div>
        <div class="ssh-row" data-pw-row>
          <label>Passwort</label><input data-pw type="password" class="grow" autocomplete="off">
        </div>
        <div class="ssh-row" data-key-row style="display:none;flex-direction:column;align-items:stretch">
          <label>Privater Schlüssel (PEM)</label>
          <textarea data-key placeholder="-----BEGIN OPENSSH PRIVATE KEY-----" spellcheck="false"></textarea>
          <div class="ssh-row"><label>Passphrase</label><input data-passphrase type="password" class="grow" placeholder="(optional)" autocomplete="off"></div>
        </div>
        <div class="ssh-row">
          <label><input type="checkbox" data-save> Als Profil speichern</label>
          <input data-save-name class="grow" placeholder="Profilname" style="display:none">
        </div>
        <div class="ssh-row">
          <button class="ssh-btn primary" data-connect-go>Verbinden</button>
          <button class="ssh-btn" data-connect-cancel>Abbrechen</button>
          <span class="ssh-stat" data-connect-msg></span>
        </div>
      </div>

      <div class="ssh-panel ssh-profiles" data-profiles>
        <ul class="ssh-plist" data-plist></ul>
        <button class="ssh-btn" data-prof-add>+ Neues Profil</button>
      </div>

      <div class="ssh-body" data-body>
        <div class="ssh-empty" data-empty>Kein Terminal verbunden.<br>Profil wählen, „Lokale Shell" oder „Neu…".</div>
      </div>
    </div>`;

  const $ = sel => host.querySelector(sel);
  const bar = {
    dot: $('.ssh-dot'), stat: $('[data-stat]'),
    prof: $('[data-prof]'), connProf: $('[data-conn-prof]'),
    local: $('[data-local]'), neu: $('[data-new]'), manage: $('[data-manage]'),
    disc: $('[data-disc]'), close: $('[data-close]'),
  };
  const bodyEl = $('[data-body]');
  const emptyEl = $('[data-empty]');
  const connectPanel = $('[data-connect]');
  const profilesPanel = $('[data-profiles]');
  const hkwarn = $('[data-hkwarn]');

  function setStatus(state, text) {
    bar.dot.className = 'ssh-dot' + (state ? ' ' + state : '');
    bar.stat.textContent = text;
    bar.stat.title = text;
  }

  function setConnected(on) {
    bar.disc.disabled = !on;
    if (on && emptyEl) emptyEl.style.display = 'none';
  }

  // --- terminal ---
  async function ensureTerm() {
    if (term) return term;
    await ensureXterm();
    term = new xtermMod.Terminal({
      cursorBlink: true, fontFamily: "'JetBrains Mono', ui-monospace, monospace",
      fontSize: 12.5, scrollback: 5000, convertEol: false,
      theme: {
        background: '#0a0e12', foreground: '#cdd9e1', cursor: '#3B82F6',
        selectionBackground: 'rgba(59,130,246,.35)',
        black: '#0a0e12', brightBlack: '#52606E',
      },
    });
    fit = new fitMod.FitAddon();
    term.loadAddon(fit);
    term.open(bodyEl);
    doFit();
    term.onData(d => { if (ws && ws.readyState === 1) ws.send(enc.encode(d)); });
    term.onResize(({ cols, rows }) => sendCtl({ t: 'resize', cols, rows }));
    ro = new ResizeObserver(() => doFit());
    ro.observe(bodyEl);
    return term;
  }

  function doFit() {
    if (!fit) return;
    try { fit.fit(); } catch (_) { /* body not laid out yet */ }
  }

  function sendCtl(obj) {
    if (ws && ws.readyState === 1) ws.send(JSON.stringify(obj));
  }

  // --- connect / disconnect ---
  async function connect(attempt) {
    await ensureTerm();
    disconnectWS();               // drop any previous session
    lastAttempt = attempt;
    hkwarn.classList.remove('open');
    connectPanel.classList.remove('open');
    profilesPanel.classList.remove('open');
    setStatus('busy', 'verbinde…');
    term.reset();

    const sock = new WebSocket(wsURL());
    sock.binaryType = 'arraybuffer';
    ws = sock;
    sock.onopen = () => {
      const dims = term ? { cols: term.cols, rows: term.rows } : {};
      sock.send(JSON.stringify(Object.assign({}, attempt, dims)));
    };
    sock.onmessage = ev => {
      if (typeof ev.data === 'string') { handleControl(ev.data); return; }
      if (term) term.write(new Uint8Array(ev.data));
    };
    sock.onclose = () => {
      if (ws === sock) { ws = null; setConnected(false); if (bar.dot.className.indexOf('err') < 0) setStatus('', 'getrennt'); }
    };
    sock.onerror = () => { /* onclose follows */ };
  }

  function handleControl(raw) {
    let m; try { m = JSON.parse(raw); } catch (_) { return; }
    if (m.t !== 'status') return;
    if (m.state === 'connected') {
      setConnected(true);
      setStatus('up', connLabel(lastAttempt));
      doFit();
      if (term) term.focus();
    } else if (m.state === 'error') {
      const detail = String(m.detail || '');
      if (detail.indexOf('hostkey_changed') === 0 && lastAttempt && lastAttempt.host) {
        showHostKeyWarning(lastAttempt.host, lastAttempt.port || 22);
      } else {
        setStatus('err', 'Fehler: ' + (detail || 'Verbindung fehlgeschlagen'));
      }
    } else if (m.state === 'closed') {
      setConnected(false);
    }
  }

  function connLabel(a) {
    if (!a) return 'verbunden';
    if (a.kind === 'local') return 'lokale Shell';
    if (a.title) return a.title;
    if (a.host) return (a.user ? a.user + '@' : '') + a.host;
    return 'verbunden';
  }

  function disconnectWS() {
    if (ws) { try { ws.close(); } catch (_) {} ws = null; }
  }

  // --- host-key TOFU re-trust ---
  function showHostKeyWarning(hHost, hPort) {
    pendingHostKey = { host: hHost, port: hPort };
    host.querySelector('[data-hkmsg]').textContent =
      `⚠ Host-Schlüssel für ${hHost}:${hPort} hat sich geändert — Verbindung blockiert.`;
    hkwarn.classList.add('open');
    setStatus('err', 'Host-Schlüssel geändert');
  }

  host.querySelector('[data-hktrust]').onclick = async () => {
    if (!pendingHostKey) return;
    try {
      await fetch('console/hostkey/forget', {
        method: 'POST', credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(pendingHostKey),
      });
    } catch (_) { /* surfaced on reconnect */ }
    hkwarn.classList.remove('open');
    if (lastAttempt) connect(lastAttempt);
  };
  host.querySelector('[data-hkcancel]').onclick = () => { hkwarn.classList.remove('open'); setStatus('', 'getrennt'); };

  // --- toolbar wiring ---
  bar.local.onclick = () => connect({ kind: 'local' });
  bar.disc.onclick = () => { disconnectWS(); setConnected(false); setStatus('', 'getrennt'); };
  bar.connProf.onclick = () => {
    const id = parseInt(bar.prof.value, 10);
    if (!id) { setStatus('err', 'kein Profil gewählt'); return; }
    const p = (profilesCache || []).find(x => x.id === id);
    connect({ kind: 'ssh', profile: id, title: p ? p.name : undefined });
  };
  bar.neu.onclick = () => togglePanel(connectPanel);
  bar.manage.onclick = async () => { await renderProfileList(); togglePanel(profilesPanel); };
  // The dock owns the column lifecycle: closing calls back so it can
  // dispose this pane and remove the column (dispose runs there).
  if (bar.close) bar.close.onclick = () => { if (opts.onClose) opts.onClose(); };

  function togglePanel(panel) {
    const open = panel.classList.contains('open');
    connectPanel.classList.remove('open');
    profilesPanel.classList.remove('open');
    if (!open) panel.classList.add('open');
  }

  // --- ad-hoc connect form ---
  const f = {
    host: $('[data-host]'), port: $('[data-port]'), user: $('[data-user]'),
    pw: $('[data-pw]'), pwRow: $('[data-pw-row]'), keyRow: $('[data-key-row]'),
    key: $('[data-key]'), passphrase: $('[data-passphrase]'),
    save: $('[data-save]'), saveName: $('[data-save-name]'), msg: $('[data-connect-msg]'),
  };
  host.querySelectorAll('[data-auth]').forEach(r => r.onchange = () => {
    const key = host.querySelector('[data-auth][value="key"]').checked;
    f.keyRow.style.display = key ? 'flex' : 'none';
    f.pwRow.style.display = key ? 'none' : 'flex';
  });
  f.save.onchange = () => { f.saveName.style.display = f.save.checked ? '' : 'none'; };
  let editId = 0; // >0 when the form is editing an existing profile
  host.querySelector('[data-connect-cancel]').onclick = () => { connectPanel.classList.remove('open'); f.msg.textContent = ''; };

  host.querySelector('[data-connect-go]').onclick = async () => {
    const authKey = host.querySelector('[data-auth][value="key"]').checked;
    const attempt = {
      kind: 'ssh', host: f.host.value.trim(),
      port: parseInt(f.port.value, 10) || 22, user: f.user.value.trim(),
      auth: authKey ? 'key' : 'password',
    };
    if (!attempt.host) { f.msg.textContent = 'Host fehlt'; return; }
    if (authKey) { attempt.key = f.key.value; attempt.passphrase = f.passphrase.value; }
    else { attempt.password = f.pw.value; }

    // Optionally persist as a profile first (or update the one being edited).
    if (f.save.checked || editId) {
      const body = {
        name: (f.saveName.value.trim() || attempt.host), host: attempt.host,
        port: attempt.port, username: attempt.user, auth_kind: attempt.auth,
        secret: authKey ? f.key.value : f.pw.value,
        passphrase: authKey ? f.passphrase.value : '',
      };
      try {
        const url = editId ? 'console/profiles/' + editId : 'console/profiles';
        const r = await fetch(url, {
          method: 'POST', credentials: 'same-origin',
          headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body),
        });
        if (!r.ok) { const e = await r.json().catch(() => ({})); f.msg.textContent = 'Profil: ' + (e.error || r.status); }
        else { await refreshProfiles(); }
      } catch (_) { f.msg.textContent = 'Profil-Speichern fehlgeschlagen'; }
    }
    editId = 0;
    connect(attempt);
  };

  // --- profile manager ---
  host.querySelector('[data-prof-add]').onclick = () => { editId = 0; resetForm(); togglePanel(connectPanel); };

  async function renderProfileList() {
    const list = await fetchProfiles();
    const ul = host.querySelector('[data-plist]');
    if (!list.length) { ul.innerHTML = '<li><span class="pmeta"><small>Noch keine Profile.</small></span></li>'; return; }
    ul.innerHTML = list.map(p => `
      <li data-id="${p.id}">
        <span class="pmeta">${esc(p.name)} <small>${esc(p.username)}@${esc(p.host)}:${p.port} · ${esc(p.auth_kind)}</small></span>
        <button class="ssh-btn" data-edit="${p.id}">Bearbeiten</button>
        <button class="ssh-btn danger" data-del="${p.id}">Löschen</button>
      </li>`).join('');
    ul.querySelectorAll('[data-edit]').forEach(b => b.onclick = () => loadProfileIntoForm(parseInt(b.dataset.edit, 10)));
    ul.querySelectorAll('[data-del]').forEach(b => b.onclick = () => deleteProfile(parseInt(b.dataset.del, 10)));
  }

  function loadProfileIntoForm(id) {
    const p = (profilesCache || []).find(x => x.id === id);
    if (!p) return;
    editId = id;
    f.host.value = p.host; f.port.value = p.port; f.user.value = p.username;
    const isKey = p.auth_kind === 'key';
    host.querySelector('[data-auth][value="key"]').checked = isKey;
    host.querySelector('[data-auth][value="password"]').checked = !isKey;
    f.keyRow.style.display = isKey ? 'flex' : 'none';
    f.pwRow.style.display = isKey ? 'none' : 'flex';
    f.pw.value = ''; f.key.value = ''; f.passphrase.value = '';
    f.save.checked = true; f.saveName.style.display = ''; f.saveName.value = p.name;
    f.msg.textContent = 'Geheimnis leer lassen = unverändert.';
    togglePanel(connectPanel);
  }

  async function deleteProfile(id) {
    try {
      await fetch('console/profiles/' + id + '/delete', { method: 'POST', credentials: 'same-origin' });
      await refreshProfiles();
      await renderProfileList();
    } catch (_) { /* ignore */ }
  }

  function resetForm() {
    f.host.value = ''; f.port.value = '22'; f.user.value = '';
    f.pw.value = ''; f.key.value = ''; f.passphrase.value = '';
    f.save.checked = false; f.saveName.style.display = 'none'; f.saveName.value = '';
    f.msg.textContent = '';
    host.querySelector('[data-auth][value="password"]').checked = true;
    f.keyRow.style.display = 'none'; f.pwRow.style.display = 'flex';
  }

  async function refreshProfiles() {
    const list = await fetchProfiles(true);
    fillProfileSelect(list);
  }

  function fillProfileSelect(list) {
    const cur = bar.prof.value;
    bar.prof.innerHTML = '<option value="">— Profil —</option>' +
      list.map(p => `<option value="${p.id}">${esc(p.name)}</option>`).join('');
    if (cur) bar.prof.value = cur;
  }

  // --- init ---
  (async () => {
    const caps = await fetchCaps();
    if (caps.local_shell) bar.local.hidden = false;
    else bar.local.title = 'Lokale Shell nur auf dem Linux-Server-Gerät verfügbar';
    if (caps.profiles) fillProfileSelect(await fetchProfiles());
  })();

  function dispose() {
    disconnectWS();
    if (ro) { ro.disconnect(); ro = null; }
    if (term) { try { term.dispose(); } catch (_) {} term = null; }
  }

  return { dispose };
}

let _uid = 0;
function uid() { return 'x' + (++_uid); }
