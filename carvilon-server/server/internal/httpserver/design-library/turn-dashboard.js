/* TURN admin dashboard (Saison 18-10). Vanilla JS, no framework. Reads
   the server-rendered initial snapshot from <script id="td-data">, then
   polls GET /a/turn/stats.json every 2s and repaints the config, live
   stats and history panels. The relay lives on the VPS; this page shows
   the last snapshot the cloud forwarded plus how fresh it is. */
(function () {
  "use strict";
  var root = document.getElementById("td-root");
  if (!root) return;
  var dataEl = document.getElementById("td-data");
  var DASH = null;
  try { DASH = JSON.parse(dataEl.textContent); } catch (e) { DASH = null; }
  var POLL = root.getAttribute("data-poll") || "/a/turn/stats.json";
  var IV = 2000;

  function esc(s) {
    return String(s == null ? "" : s).replace(/[&<>"']/g, function (c) {
      return ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c];
    });
  }
  function el(id) { return document.getElementById(id); }
  function fmtTime(iso) {
    if (!iso) return "";
    var d = new Date(iso);
    return isNaN(d.getTime()) ? esc(iso) : d.toLocaleString("de");
  }
  function fmtSince(iso) {
    if (!iso) return "";
    var d = new Date(iso);
    if (isNaN(d.getTime())) return "";
    var s = Math.max(0, Math.floor((Date.now() - d.getTime()) / 1000));
    var m = Math.floor(s / 60);
    if (m < 1) return s + "s";
    var h = Math.floor(m / 60);
    return h < 1 ? m + "m" : h + "h " + (m % 60) + "m";
  }
  function kv(k, v) {
    return '<div class="td-kv"><div class="k">' + esc(k) + '</div><div class="v">' + esc(v) + "</div></div>";
  }

  function renderFresh(d) {
    var box = el("td-fresh"), txt = el("td-fresh-txt");
    box.className = "td-fresh";
    if (!d || !d.snapshot_present) {
      txt.textContent = "Noch kein Stand von der Cloud empfangen (TURN nicht aktiv oder Cloud nicht verbunden).";
      return;
    }
    if (d.stale) {
      box.classList.add("stale");
      txt.textContent = "Stand veraltet (vor " + d.age_seconds + "s) - Cloud nicht erreichbar.";
      return;
    }
    box.classList.add("ok");
    txt.textContent = "Aktueller Stand (vor " + d.age_seconds + "s).";
  }

  function renderConfig(d) {
    var s = (d && d.stats) || {};
    var state = d && d.active ? "aktiv" : (s.enabled ? "konfiguriert" : "aus");
    var cert = s.cert_mode === "separate" ? "eigenes Zertifikat"
      : (s.cert_mode === "shared" ? "WHIP-Zertifikat" : "TLS aus");
    var html = "";
    html += kv("TURN", state);
    html += kv("STUN", s.stun_active ? "aktiv" : "aus");
    html += kv("UDP-Port", s.udp_port || "-");
    html += kv("TLS-Port", s.tls_port ? s.tls_port : "aus");
    html += kv("Realm", s.realm || "-");
    html += kv("turns-Host", s.turns_host || "-");
    html += kv("Cred-TTL", s.cred_ttl_seconds ? s.cred_ttl_seconds + "s" : "-");
    html += kv("Zertifikat", cert);
    el("td-config").innerHTML = html;
  }

  function renderClients(d) {
    var s = (d && d.stats) || {};
    var alloc = s.allocation_count || 0;
    var clients = (d && d.active && s.clients) || [];
    var head = '<div class="td-allocline">Aktive Allokationen: <b>' + esc(alloc) + "</b></div>";
    if (!clients.length) {
      el("td-clients").innerHTML = head + '<div class="td-empty">Keine aktiven Allokationen.</div>';
      return;
    }
    var rows = clients.map(function (c) {
      return "<tr><td class=\"td-mono\">" + esc(c.src_masked) + "</td><td>" + esc(c.username || "")
        + "</td><td>" + esc(fmtSince(c.since)) + "</td></tr>";
    }).join("");
    el("td-clients").innerHTML = head
      + '<table class="td-table"><thead><tr><th>Client (gekürzt)</th><th>Username</th><th>Seit</th></tr></thead><tbody>'
      + rows + "</tbody></table>";
  }

  function eventDetail(e) {
    if (e.kind === "auth") return e.auth_ok ? "ok" : "abgelehnt";
    if (e.kind === "allocation_error") return e.err || "";
    return "";
  }

  function renderEvents(d) {
    var ev = (d && d.events) || [];
    if (!ev.length) {
      el("td-events").innerHTML = '<div class="td-empty">Noch keine TURN-Ereignisse.</div>';
      return;
    }
    var rows = ev.map(function (e) {
      return "<tr><td>" + esc(fmtTime(e.time)) + '</td><td><span class="td-pill">' + esc(e.kind)
        + "</span></td><td>" + esc(e.protocol || "") + "</td><td class=\"td-mono\">" + esc(e.src_masked || "")
        + "</td><td>" + esc(e.realm || "") + "</td><td>" + esc(eventDetail(e)) + "</td></tr>";
    }).join("");
    el("td-events").innerHTML =
      '<table class="td-table"><thead><tr><th>Zeit</th><th>Ereignis</th><th>Proto</th><th>Client (gekürzt)</th><th>Realm</th><th>Detail</th></tr></thead><tbody>'
      + rows + "</tbody></table>";
  }

  function renderICE(d) {
    var ice = (d && d.ice_events) || [];
    if (!ice.length) {
      el("td-ice").innerHTML = '<div class="td-empty">Noch keine ICE-Ereignisse.</div>';
      return;
    }
    var rows = ice.map(function (e) {
      return "<tr><td>" + esc(fmtTime(e.time)) + "</td><td class=\"td-mono\">" + esc(e.stream_id)
        + "</td><td>" + esc(e.state) + '</td><td class="num">' + esc(e.since_start_ms) + " ms</td></tr>";
    }).join("");
    el("td-ice").innerHTML =
      '<table class="td-table"><thead><tr><th>Zeit</th><th>Stream</th><th>Status</th><th class="num">Seit Start</th></tr></thead><tbody>'
      + rows + "</tbody></table>";
  }

  function render(d) {
    renderFresh(d);
    renderConfig(d);
    renderClients(d);
    renderEvents(d);
    renderICE(d);
    var note = el("td-note");
    if (note) note.textContent = (d && d.note) || "";
  }

  function poll() {
    fetch(POLL, { headers: { Accept: "application/json" }, credentials: "same-origin" })
      .then(function (r) { return r.ok ? r.json() : null; })
      .then(function (j) { if (j) render(j); })
      .catch(function () { /* keep last paint; next tick retries */ });
  }

  if (DASH) render(DASH);
  setInterval(poll, IV);
  poll();
})();
