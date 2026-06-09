/* Streams dashboard (Saison 17-14). Vanilla JS, no framework, no
   localStorage. Reads the server-rendered initial snapshot from
   <script id="sd-data">, then polls GET /a/streams/stats.json every
   2/5/10s and updates the three tiers in place (preserving expand
   state). Sparklines accumulate the recent poll history. Behaviour and
   density follow streams-dashboard-mockup.html. */
(function () {
  "use strict";
  var root = document.getElementById("sd-root");
  if (!root) return;
  var dataEl = document.getElementById("sd-data");
  var DASH;
  try { DASH = JSON.parse(dataEl.textContent); } catch (e) { return; }
  if (!DASH || !DASH.configured) return;

  var POLL_URL = root.getAttribute("data-poll") || "/a/streams/stats.json";
  var IV = 2000, timer = null, started = false;
  var HIST = {};            // profile name -> { eg:[], fps:[] }
  var GHIST = [];           // global egress history
  var HIST_LEN = 26;

  // ---- helpers ----
  var $ = function (s, r) { return (r || document).querySelector(s); };
  var ce = function (t, c) { var e = document.createElement(t); if (c) e.className = c; return e; };
  function fmtBytes(b) { if (!b || b <= 0) return "0 B"; var u = ["B", "KB", "MB", "GB", "TB"], i = Math.floor(Math.log(b) / Math.log(1024)); return (b / Math.pow(1024, i)).toFixed(i ? 1 : 0) + " " + u[i]; }
  function fmtMbit(k) { return ((k || 0) / 1000).toFixed(2); }
  function fmtUp(s) { s = Math.max(0, s | 0); var m = Math.floor(s / 60), ss = s % 60; if (m >= 60) { var h = Math.floor(m / 60); return h + "h " + (m % 60) + "m"; } return m + "m " + String(ss).padStart(2, "0") + "s"; }
  function num(n) { return (n || 0).toLocaleString("de"); }
  function codecClass(c) { if (c === "mjpeg") return "sd-cb-mjpeg"; if (c === "h264_cbp") return "sd-cb-cbp"; return "sd-cb-h264"; }
  function codecColor(c) { if (c === "mjpeg") return "var(--sd-amber)"; if (c === "h264_cbp") return "var(--sd-violet)"; return "var(--sd-blue)"; }
  function codecShort(c) { return (c || "").replace("_passthrough", ""); }
  function gaugeTarget(p) { return p.target_fps > 0 ? p.target_fps : Math.round(p.source_fps || p.avg_fps || 0); }
  function sparkPath(arr, w, h, pad) {
    pad = pad || 2; if (!arr.length) return "";
    var mn = Math.min.apply(null, arr), mx = Math.max.apply(null, arr), rg = (mx - mn) || 1, st = (w - pad * 2) / (Math.max(1, arr.length - 1));
    return arr.map(function (v, i) { return (i ? "L" : "M") + (pad + i * st).toFixed(1) + " " + (h - pad - ((v - mn) / rg) * (h - pad * 2)).toFixed(1); }).join(" ");
  }
  function seed(base) { var a = []; for (var i = 0; i < HIST_LEN; i++) a.push(base || 0); return a; }
  function pushHist(arr, v) { arr.push(v); while (arr.length > HIST_LEN) arr.shift(); }
  function countUp(el, to, fmt) {
    fmt = fmt || function (v) { return Math.round(v); }; var t0 = null;
    function step(t) { if (t0 === null) t0 = t; var k = Math.min(1, (t - t0) / 850), e = 1 - Math.pow(1 - k, 3); el.textContent = fmt(to * e); if (k < 1) requestAnimationFrame(step); }
    requestAnimationFrame(step);
  }

  var I = {
    stream: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M4.5 9a8 8 0 0 1 15 0"/><path d="M7.5 11.5a4.5 4.5 0 0 1 9 0"/><circle cx="12" cy="15" r="1.6"/></svg>',
    users: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M16 19v-1a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v1"/><circle cx="9" cy="7" r="3"/><path d="M22 19v-1a4 4 0 0 0-3-3.8"/></svg>',
    flow: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M3 12h4l3-8 4 16 3-8h4"/></svg>',
    film: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="3" y="4" width="18" height="16" rx="2"/><path d="M7 4v16M17 4v16M3 9h4M3 15h4M17 9h4M17 15h4"/></svg>',
    shield: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 3l7 3v6c0 4.4-3 7.5-7 9-4-1.5-7-4.6-7-9V6z"/><path d="M9 12l2 2 4-4"/></svg>',
    gauge: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 14l4-4"/><path d="M5.5 18a8 8 0 1 1 13 0"/></svg>',
    list: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M8 6h13M8 12h13M8 18h13M3 6h.01M3 12h.01M3 18h.01"/></svg>',
    monitor: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="3" y="4" width="18" height="12" rx="2"/><path d="M8 20h8M12 16v4"/></svg>',
    globe: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="9"/><path d="M3 12h18M12 3a14 14 0 0 1 0 18 14 14 0 0 1 0-18"/></svg>',
    loop: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M17 2l4 4-4 4"/><path d="M3 11V9a4 4 0 0 1 4-4h14"/><path d="M7 22l-4-4 4-4"/><path d="M21 13v2a4 4 0 0 1-4 4H3"/></svg>',
    chev: '<svg viewBox="0 0 24 24" width="15" height="15" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round"><path d="M9 6l6 6-6 6"/></svg>',
    zzz: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M4 8h7l-7 8h7"/><path d="M14 4h6l-6 7h6"/></svg>'
  };
  var KIND = {
    esp: { ico: I.monitor, tag: "ESP", tcls: "sd-tag-esp", icls: "sd-ci-esp" },
    web: { ico: I.globe, tag: "Browser", tcls: "sd-tag-web", icls: "sd-ci-web" },
    loop: { ico: I.loop, tag: "Loopback", tcls: "sd-tag-loop", icls: "sd-ci-loop" }
  };

  function ensureHist(p) { if (!HIST[p.name]) HIST[p.name] = { eg: seed(p.avg_bitrate_kbps), fps: seed(p.avg_fps) }; return HIST[p.name]; }

  // ---- summary ----
  function aggregate(d) {
    var active = 0, consumers = d.global.clients || 0, egress = 0, frames = d.global.frames_sent_total || 0, dropped = 0;
    d.profiles.forEach(function (p) { if (p.active) { active++; egress += p.avg_bitrate_kbps * p.clients; dropped += p.frames_dropped; } });
    return { active: active, total: d.profiles.length, consumers: consumers, egress: egress, frames: frames, dropped: dropped };
  }
  function buildSummary(d) {
    var sm = $(".sd-summary", root); sm.innerHTML = "";
    var a = aggregate(d);
    GHIST = seed(a.egress);
    var cards = [
      { k: "active", ic: I.stream, ac: "var(--sd-live)", label: "Aktive Streams", val: a.active, foot: "von " + a.total + " Profilen" },
      { k: "consumers", ic: I.users, ac: "var(--sd-blue)", label: "LAN-Konsumenten", val: a.consumers, foot: "verbunden · LAN" },
      { k: "egress", ic: I.flow, ac: "var(--sd-violet)", label: "Egress gesamt", val: fmtMbit(a.egress), unit: "Mbit/s", spark: true, foot: "aggregiert" },
      { k: "frames", ic: I.film, ac: "var(--sd-amber)", label: "Frames gesendet", val: a.frames, foot: "seit Session-Start" },
      { k: "dropped", ic: I.shield, ac: "var(--sd-live)", label: "Verlorene Frames", val: a.dropped, health: true, foot: "alle Pfade" }
    ];
    cards.forEach(function (c, i) {
      var el = ce("div", "sd-card" + (c.health ? " sd-health" : "")); el.style.animationDelay = (i * 55) + "ms"; el.dataset.k = c.k;
      var bar = ce("div", "sd-accent"); bar.style.background = c.ac; bar.style.boxShadow = "0 0 14px -2px " + c.ac; el.appendChild(bar);
      var lab = ce("div", "sd-clabel"); lab.innerHTML = '<span style="color:' + c.ac + '">' + c.ic + "</span>" + c.label; el.appendChild(lab);
      var val = ce("div", "sd-cval"); el.appendChild(val);
      if (c.unit) { var n = ce("span"); val.appendChild(n); var u = ce("span", "sd-unit"); u.textContent = " " + c.unit; val.appendChild(u); val.dataset.live = "egress"; countUp(n, parseFloat(c.val), function (v) { return v.toFixed(2); }); }
      else { val.dataset.live = c.k; countUp(val, Number(c.val), null); }
      if (c.spark) {
        var ns = "http://www.w3.org/2000/svg", svg = document.createElementNS(ns, "svg");
        svg.setAttribute("class", "sd-spark"); svg.setAttribute("viewBox", "0 0 160 28"); svg.setAttribute("preserveAspectRatio", "none");
        var gid = "sdg" + i;
        svg.innerHTML = '<defs><linearGradient id="' + gid + '" x1="0" y1="0" x2="0" y2="1"><stop offset="0" stop-color="' + c.ac + '" stop-opacity=".35"/><stop offset="1" stop-color="' + c.ac + '" stop-opacity="0"/></linearGradient></defs>';
        var area = document.createElementNS(ns, "path"), line = document.createElementNS(ns, "path"), dd = sparkPath(GHIST, 160, 28);
        line.setAttribute("d", dd); line.setAttribute("fill", "none"); line.setAttribute("stroke", c.ac); line.setAttribute("stroke-width", "1.6"); line.setAttribute("stroke-linejoin", "round");
        area.setAttribute("d", dd + " L158 26 L2 26 Z"); area.setAttribute("fill", "url(#" + gid + ")");
        svg.appendChild(area); svg.appendChild(line); el.appendChild(svg); el._line = line; el._area = area;
      } else {
        var f = ce("div", "sd-cfoot");
        if (c.health) f.innerHTML = '<span class="sd-badge-ok"><span class="sd-ldot" style="width:6px;height:6px"></span>Stabil</span>';
        else f.textContent = c.foot;
        el.appendChild(f);
      }
      sm.appendChild(el);
    });
    appendCloudCard(sm, d);
  }
  function updateSummary(d) {
    var a = aggregate(d);
    setLive("active", a.active); setLive("consumers", a.consumers); setLive("frames", a.frames);
    refreshCloudCard(d);
    var egEl = $('.sd-cval[data-live="egress"] span', root); if (egEl) egEl.textContent = fmtMbit(a.egress);
    pushHist(GHIST, a.egress);
    var card = $('.sd-card[data-k="egress"]', root);
    if (card && card._line) { var dd = sparkPath(GHIST, 160, 28); card._line.setAttribute("d", dd); card._area.setAttribute("d", dd + " L158 26 L2 26 Z"); }
    var dCard = $('.sd-card[data-k="dropped"]', root);
    if (dCard) { var v = $(".sd-cval", dCard); if (v) v.textContent = a.dropped; var foot = $(".sd-cfoot .sd-badge-ok", dCard); if (foot) foot.innerHTML = (a.dropped === 0 ? '<span class="sd-ldot" style="width:6px;height:6px"></span>Stabil' : "⚠ " + a.dropped + " verloren"); }
  }
  function setLive(k, v) { var el = $('.sd-cval[data-live="' + k + '"]', root); if (el) el.textContent = num(v); }

  // ---- cloud viewers (S20 step 2) ----
  // The VPS pushes per-stream WHEP-subscriber counts over the side-channel;
  // carvilon serves them on d.cloud (present/stale/age_seconds/total) and per
  // profile (p.cloud_clients). Rendered SEPARATELY from the LAN count; a stale
  // snapshot shows "veraltet" instead of a misleading fresh number.
  function cloudChip(p) {
    var c = DASH.cloud || {};
    if (!c.present) return "";
    if (c.stale) return '<span class="sd-cloud stale" title="Cloud-Konsumenten veraltet">☁ –</span>';
    return '<span class="sd-cloud" title="Cloud-Konsumenten">☁ ' + (p.cloud_clients || 0) + '</span>';
  }
  function cloudSummary(d) {
    var c = d.cloud || {};
    if (!c.present) return { val: "–", foot: "keine Cloud-Daten", muted: true };
    var age = c.age_seconds | 0;
    if (c.stale) return { val: "–", foot: "veraltet · vor " + age + "s", muted: true };
    return { val: String(c.total | 0), foot: "Stand vor " + age + "s", muted: false };
  }
  function appendCloudCard(sm, d) {
    var cv = cloudSummary(d), ac = "var(--sd-violet)";
    var el = ce("div", "sd-card" + (cv.muted ? " sd-muted" : "")); el.dataset.k = "cloud";
    var bar = ce("div", "sd-accent"); bar.style.background = ac; bar.style.boxShadow = "0 0 14px -2px " + ac; el.appendChild(bar);
    var lab = ce("div", "sd-clabel"); lab.innerHTML = '<span style="color:' + ac + '">' + I.globe + "</span>Cloud-Konsumenten"; el.appendChild(lab);
    var val = ce("div", "sd-cval"); val.dataset.live = "cloud"; val.textContent = cv.val; el.appendChild(val);
    var f = ce("div", "sd-cfoot sd-cloudfoot"); f.textContent = cv.foot; el.appendChild(f);
    sm.appendChild(el);
  }
  function refreshCloudCard(d) {
    var el = $('.sd-card[data-k="cloud"]', root); if (!el) return;
    var cv = cloudSummary(d);
    el.classList.toggle("sd-muted", cv.muted);
    var v = $('.sd-cval', el); if (v) v.textContent = cv.val;
    var f = $('.sd-cloudfoot', el); if (f) f.textContent = cv.foot;
  }

  // ---- rows ----
  function gaugeSVG(p) {
    var r = 12, c = 2 * Math.PI * r, t = gaugeTarget(p), pct = t ? Math.min(1, p.avg_fps / t) : 0;
    var col = pct >= .92 ? "var(--sd-live)" : pct >= .6 ? "var(--sd-amber)" : "var(--sd-red)";
    return '<svg class="sd-gring" viewBox="0 0 28 28"><circle class="bg" cx="14" cy="14" r="' + r + '" fill="none" stroke-width="3"/>' +
      '<circle class="fg" cx="14" cy="14" r="' + r + '" fill="none" stroke-width="3" stroke="' + col + '" stroke-dasharray="' + c + '" stroke-dashoffset="' + (c * (1 - pct)) + '"/></svg>';
  }
  function miniSpark(arr, color) { return '<svg class="sd-minispark" viewBox="0 0 74 16" preserveAspectRatio="none"><path d="' + sparkPath(arr, 74, 16, 1) + '" fill="none" stroke="' + color + '" stroke-width="1.4" stroke-linejoin="round" opacity=".9"/></svg>'; }

  function buildRows(d) {
    var rows = $(".sd-rows", root); rows.innerHTML = ""; $(".sd-count", root).textContent = d.profiles.length;
    d.profiles.forEach(function (p, idx) {
      ensureHist(p);
      var row = ce("div", "sd-trow" + (p.active ? "" : " idle")); row.style.animationDelay = (90 + idx * 45) + "ms"; row.dataset.name = p.name;
      var m = ce("div", "sd-rmain"); m.innerHTML = rowMainHTML(p); row.appendChild(m);
      var det = ce("div", "sd-rdetail"), din = ce("div", "sd-rdetail-in"), body = ce("div", "sd-detail-body");
      body.innerHTML = detailHTML(p); din.appendChild(body); det.appendChild(din); row.appendChild(det);
      // Every row is expandable (active rows -> live metrics + clients,
      // idle rows -> idle note); both expose the manage actions, so the
      // admin can still edit/delete an idle profile.
      m.addEventListener("click", function (e) { if (e.target.closest("a,button,form")) return; row.classList.toggle("open"); });
      rows.appendChild(row);
    });
  }
  function rowMainHTML(p) {
    var cc = codecColor(p.codec), h = ensureHist(p);
    return '' +
      '<div class="sd-pname"><span class="sd-cbadge ' + codecClass(p.codec) + '">' + esc(codecShort(p.codec)) + '</span>' +
      '<span style="min-width:0"><div class="nm">' + esc(p.name) + '</div><div class="meta">' + esc(p.description || p.usage || "") + '</div></span></div>' +
      '<div class="sd-status ' + (p.active ? "live" : "idle") + '"><span class="sd-sd"></span>' + (p.active ? "aktiv" : "idle") + '</div>' +
      '<div class="r sd-consumers ' + (p.clients ? "" : "zero") + '"><span class="sd-lan">' + p.clients + '</span>' + cloudChip(p) + '</div>' +
      '<div class="r sd-col-src sd-metric ' + (p.active ? "" : "dim") + '">' + (p.active ? p.source_fps.toFixed(1) + '<span class="u">fps</span>' : "–") + '</div>' +
      '<div class="r sd-col-fps sd-gauge">' + (p.active ? '<div class="g-num">' + p.avg_fps.toFixed(1) + '<br><small>/ ' + gaugeTarget(p) + ' fps</small></div>' + gaugeSVG(p) : '<span class="sd-metric dim">–</span>') + '</div>' +
      '<div class="r sd-col-eg sd-egress">' + (p.active ? '<div class="ev">' + fmtMbit(p.avg_bitrate_kbps) + '<span class="u"> Mbit/s</span></div>' + miniSpark(h.eg, cc) : '<span class="sd-metric dim">–</span>') + '</div>' +
      '<div class="r sd-col-h sd-health">' + (p.active ? '<span class="sd-hpill ' + (p.frames_dropped ? "sd-h-warn" : "sd-h-ok") + '">' + (p.frames_dropped ? "⚠ " : "✓ ") + p.frames_dropped + ' Drops</span>' : '<span class="sd-hpill sd-h-idle">–</span>') + '</div>' +
      '<div class="sd-chev">' + I.chev + '</div>';
  }
  function manageHTML(p) {
    return '<div class="sd-manage"><a class="sd-mbtn" href="/a/streams/' + encodeURIComponent(p.name) + '">Bearbeiten</a>' +
      '<form method="post" action="/a/streams/' + encodeURIComponent(p.name) + '/delete" style="display:inline;margin:0" onsubmit="return confirm(\'Profil wirklich löschen?\');">' +
      '<button type="submit" class="sd-mbtn sd-mbtn-danger">Löschen</button></form></div>';
  }
  function detailHTML(p) {
    if (!p.active) return '<div class="sd-empty-note">' + I.zzz + '<span>Kein Konsument verbunden. Dieses Profil zieht lazy – die Quelle startet erst beim ersten Subscriber.</span></div>' + manageHTML(p);
    var cc = codecColor(p.codec), h = ensureHist(p);
    var left = '<div class="sd-dblock"><h5>' + I.gauge + ' Profil-Metriken</h5><div class="sd-mgrid">' +
      mcell("Ausgang fps", p.avg_fps.toFixed(2) + '<span class="u">/ ' + gaugeTarget(p) + '</span>') +
      mcell("Quell-fps", p.source_fps.toFixed(2)) +
      mcell("Bitrate", fmtMbit(p.avg_bitrate_kbps) + '<span class="u">Mbit/s</span>') +
      mcell("Frames gesendet", num(p.frames_sent)) +
      mcell("Frames verloren", p.frames_dropped, p.frames_dropped === 0) +
      mcell("Daten", fmtBytes(p.bytes_sent)) +
      '</div><div class="sd-chartcard"><div class="ch-h"><span class="t">Verlauf · fps &amp; Egress</span>' +
      '<span class="leg"><span style="color:var(--sd-live)"><i style="background:var(--sd-live)"></i>fps</span>' +
      '<span style="color:' + cc + '"><i style="background:' + cc + '"></i>Mbit/s</span></span></div>' +
      // S17-17: draw the accumulated history (fps + egress) into the
      // detail chart. /stream/stats has no history, so each line is the
      // client-side ring buffer (h.fps / h.eg), normalized independently
      // to its own range. Recomputed on every poll re-render -> grows live.
      '<svg class="sd-bigchart" viewBox="0 0 320 90" preserveAspectRatio="none">' +
      '<path d="' + sparkPath(h.fps, 320, 90, 4) + '" fill="none" stroke="var(--sd-live)" stroke-width="1.7" stroke-linejoin="round" stroke-linecap="round"/>' +
      '<path d="' + sparkPath(h.eg, 320, 90, 4) + '" fill="none" stroke="' + cc + '" stroke-width="1.7" stroke-linejoin="round" stroke-linecap="round"/>' +
      '</svg></div></div>';
    var clients = (DASH.clients || []).filter(function (c) { return c.profile === p.name; });
    var right = '<div class="sd-dblock"><h5>' + I.list + ' Verbundene Konsumenten · ' + p.clients + '</h5><div class="sd-clients">' +
      clients.map(clientHTML).join("") + '</div></div>';
    return '<div class="sd-dgrid">' + left + right + '</div>' + manageHTML(p);
  }
  function mcell(label, val, good) { return '<div class="sd-mcell"><div class="ml">' + label + '</div><div class="mv' + (good ? " good" : "") + '">' + val + '</div></div>'; }
  function clientHTML(c) {
    var k = KIND[c.kind] || KIND.web;
    return '<div class="sd-client"><div class="sd-cl-ico ' + k.icls + '">' + k.ico + '</div><div class="sd-cl-main">' +
      '<div class="sd-cl-top"><span class="sd-cl-addr">' + esc(c.remote_addr) + '</span><span class="sd-cl-tag ' + k.tcls + '">' + k.tag + '</span></div>' +
      '<div class="sd-cl-sub"><span>up <b>' + fmtUp(c.uptime_sec) + '</b></span><span><b>' + (c.avg_fps || 0).toFixed(1) + '</b> fps</span><span><b>' + fmtMbit(c.avg_bitrate_kbps) + '</b> Mbit/s</span><span><b>' + fmtBytes(c.bytes_sent) + '</b></span></div>' +
      '</div><div class="sd-cl-fresh"><span class="sd-freshdot"></span><span class="fl">live</span></div></div>';
  }
  function esc(s) { return String(s == null ? "" : s).replace(/[&<>"]/g, function (m) { return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[m]; }); }

  // ---- update existing rows in place (preserve expand state) ----
  function updateRows(d) {
    var byName = {}; d.profiles.forEach(function (p) { byName[p.name] = p; });
    var rows = root.querySelectorAll(".sd-trow");
    Array.prototype.forEach.call(rows, function (row) {
      var p = byName[row.dataset.name]; if (!p) return;
      ensureHist(p);
      if (p.active) { pushHist(HIST[p.name].eg, p.avg_bitrate_kbps); pushHist(HIST[p.name].fps, p.avg_fps); }
      var wasOpen = row.classList.contains("open");
      // Preserve expand state across polls; the .sd-rmain click listener
      // lives on the element and survives innerHTML updates.
      row.className = "sd-trow" + (p.active ? "" : " idle") + (wasOpen ? " open" : "");
      $(".sd-rmain", row).innerHTML = rowMainHTML(p);
      $(".sd-detail-body", row).innerHTML = detailHTML(p);
    });
  }

  // ---- poll ----
  function applyData(d) { DASH = d; updateSummary(d); updateRows(d); stamp(); }
  function poll() {
    fetch(POLL_URL, { headers: { "Accept": "application/json" }, credentials: "same-origin" })
      .then(function (r) { return r.ok ? r.json() : null; })
      .then(function (d) { if (d && d.configured) applyData(d); else stamp(true); })
      .catch(function () { stamp(true); });
  }
  function stamp(stale) {
    var el = $("#sd-livetxt", root); if (!el) return;
    var now = new Date();
    el.textContent = stale ? "Stream-Server nicht erreichbar" : "live · " + now.toLocaleTimeString("de");
  }
  function startTimer() { if (timer) clearInterval(timer); timer = setInterval(poll, IV); }

  // ---- init ----
  buildSummary(DASH); buildRows(DASH);
  var seg = $(".sd-seg", root);
  if (seg) seg.addEventListener("click", function (e) {
    var b = e.target.closest("button"); if (!b) return;
    var on = $(".sd-seg .on", root); if (on) on.classList.remove("on"); b.classList.add("on");
    IV = (parseInt(b.dataset.iv, 10) || 2) * 1000; startTimer();
  });
  startTimer();
})();
