/* Sensor history charts (H2) - the STORED, interval-averaged path.
 *
 * ONE component, mounted by BOTH homes: the devices cockpit (a classic-script
 * admin page) and the Logic Editor's sensor block (an ES-module app in an
 * iframe). It is served from /static/, which both can reach, and is a classic
 * script exposing a global rather than an ES module - the same shape lucide
 * already uses in the editor - so neither host has to change how it loads
 * scripts.
 *
 * It reads the H1 query API and nothing else:
 *   GET /a/devices/sensors/metrics?device=       -> what is chartable/recorded
 *   GET /a/devices/sensors/history?device=&metric=&from=&to=&points=
 *
 * This is the stored path and only the stored path. The live reading is a
 * different, event-driven channel (the faceplate value, the cockpit readout,
 * what the climate loop evaluates) and is deliberately not touched here: the
 * two must never be conflated.
 *
 * Usage:
 *   var c = window.CarvilonSensorChart.mount(hostEl, {device: '<id>'});
 *   c.destroy();
 */
(function () {
  'use strict';

  var LOC = 'en-GB'; // admin UI language is English; 24h clock
  var DAY = 86400000;

  var RANGES = [
    { key: '1d', label: '1d', ms: DAY },
    { key: '3d', label: '3d', ms: 3 * DAY },
    { key: '1w', label: '1w', ms: 7 * DAY },
    { key: '1m', label: '1m', ms: 30 * DAY },
    { key: 'all', label: 'all', ms: 0 }
  ];

  var uidSeq = 0;

  /* seriesVar maps a metric to its fixed hue. Colour follows the QUANTITY, not
     the row order: a temperature is orange in both homes and stays orange when
     the admin accent changes, so the reader learns the mapping once. The hues
     themselves are defined (and validated) in sensor-chart.css. */
  function seriesVar(metric, kind) {
    if (/temp/i.test(metric)) return '--ch-s-temp';
    if (/_power$/.test(metric)) return '--ch-s-power';
    if (/_voltage$/.test(metric)) return '--ch-s-voltage';
    if (/_current$/.test(metric)) return '--ch-s-current';
    if (/_freq$/.test(metric)) return '--ch-s-freq';
    if (metric === 'humidity') return '--ch-s-humidity';
    if (metric === 'illuminance') return '--ch-s-light';
    if (metric === 'battery') return '--ch-s-battery';
    if (kind === 'bool') return '--ch-s-state';
    return '--ch-s-humidity';
  }

  function el(tag, cls) {
    var e = document.createElement(tag);
    if (cls) e.className = cls;
    return e;
  }

  function svgEl(tag, attrs) {
    var e = document.createElementNS('http://www.w3.org/2000/svg', tag);
    for (var k in attrs) if (attrs.hasOwnProperty(k)) e.setAttribute(k, attrs[k]);
    return e;
  }

  /* niceStep rounds a raw step up to the nearest 1/2/5 x 10^k so the y axis
     carries clean numbers (0 / 0.5 / 1), which is what makes it readable as
     the values the line isn't directly labelled with. */
  function niceStep(raw) {
    if (!(raw > 0)) return 1;
    var mag = Math.pow(10, Math.floor(Math.log(raw) / Math.LN10));
    var n = raw / mag;
    return (n <= 1 ? 1 : n <= 2 ? 2 : n <= 5 ? 5 : 10) * mag;
  }

  function yScale(min, max, want) {
    if (!isFinite(min) || !isFinite(max)) return { min: 0, max: 1, ticks: [0, 1] };
    if (min === max) { min -= 0.5; max += 0.5; }
    var step = niceStep((max - min) / Math.max(1, want));
    var lo = Math.floor(min / step) * step;
    var hi = Math.ceil(max / step) * step;
    var ticks = [];
    // Integer stepping: repeated float addition drifts and can emit 0.30000004.
    var n = Math.round((hi - lo) / step);
    for (var i = 0; i <= n; i++) ticks.push(lo + i * step);
    return { min: lo, max: hi, ticks: ticks, step: step };
  }

  function decimalsFor(step) {
    if (!(step > 0)) return 1;
    var d = Math.ceil(-Math.log(step) / Math.LN10);
    return Math.max(0, Math.min(3, isFinite(d) ? d : 1));
  }

  function fmtNum(v, dec) {
    return v.toFixed(dec);
  }

  /* Axis ticks and VALUES want different precision. A tick is rounded to the
     axis step so the scale reads 18 / 20 / 22; a reading is the thing the user
     came for and must not inherit that rounding - a 2-degree step would render
     21.4 C as "21". Precision follows the magnitude of the value instead. */
  function valDec(v, kind) {
    if (kind === 'bool') return 0; // a duty percentage; tenths are noise
    var a = Math.abs(v);
    if (a >= 100) return 0;
    if (a >= 1) return 1;
    return 2;
  }

  /* Tick labels follow the SPAN of what is shown, not the range button: "all"
     can be two hours or two years and must label itself correctly either way. */
  function tickFmt(span) {
    if (span <= 36 * 3600000) return { hour: '2-digit', minute: '2-digit' };
    if (span <= 10 * DAY) return { weekday: 'short', hour: '2-digit', minute: '2-digit' };
    if (span <= 300 * DAY) return { day: 'numeric', month: 'short' };
    return { month: 'short', year: 'numeric' };
  }

  /* How much horizontal room one tick label needs, INCLUDING breathing room.
     It has to follow the chosen format, not a single guess: "20:30" is about
     34px but "Wed 20:30" is nearly 60px, and budgeting the narrow number for
     the wide label collides the axis in the 296px editor sidebar. */
  function tickBudget(fmt, narrow) {
    if (fmt.weekday) return narrow ? 86 : 96; // "Wed 20:30"
    if (fmt.year) return narrow ? 74 : 84;    // "Jul 2026"
    if (fmt.hour) return narrow ? 54 : 64;    // "20:30"
    return narrow ? 60 : 70;                  // "15 Jul"
  }

  var TIP_FMT = { weekday: 'short', day: 'numeric', month: 'short', hour: '2-digit', minute: '2-digit' };

  function fmtTime(ts, opts) {
    try { return new Intl.DateTimeFormat(LOC, opts).format(new Date(ts)); }
    catch (e) { return new Date(ts).toISOString(); }
  }

  function fetchJSON(url, signal) {
    return fetch(url, { credentials: 'same-origin', signal }).then(function (r) {
      if (!r.ok) throw new Error('HTTP ' + r.status);
      return r.json();
    });
  }

  /* ---------------------------------------------------------------- figure */

  function makeFigure(m) {
    var uid = 'cvc' + (++uidSeq);
    var fig = el('figure', 'cvchart-fig');
    fig.setAttribute('data-metric', m.metric);
    var v = seriesVar(m.metric, m.kind);
    fig.style.setProperty('--ch-series', 'var(' + v + ')');

    // A bool metric is stored as a duty FRACTION (0..1 averaged over the
    // bucket) and is shown as a percentage. Settle that here, before the
    // caption is built, so the unit chip and the values agree - deciding it
    // later left the caption with no unit while the readings said "%".
    var unit = m.unit || (m.kind === 'bool' ? '%' : '');

    var cap = el('figcaption', 'cvchart-cap');
    cap.appendChild(el('span', 'cvchart-dot'));
    var name = el('span', 'cvchart-name');
    // textContent, never innerHTML: labels and units come from the device
    // catalog (vendor data) and stay inert markup-wise.
    name.textContent = m.label || m.metric;
    cap.appendChild(name);
    if (unit) {
      var u = el('span', 'cvchart-unit');
      u.textContent = unit;
      cap.appendChild(u);
    }
    var last = el('span', 'cvchart-last');
    last.textContent = '--';
    cap.appendChild(last);
    fig.appendChild(cap);

    var plot = el('div', 'cvchart-plot');
    var tip = el('div', 'cvchart-tip');
    tip.hidden = true;
    plot.appendChild(tip);
    fig.appendChild(plot);

    return {
      metric: m.metric, label: m.label || m.metric, unit: unit, kind: m.kind,
      uid: uid, el: fig, plot: plot, tip: tip, last: last,
      samples: null, failed: false, xMin: 0, xMax: 0, geom: null, svg: null, hover: -1
    };
  }

  /* draw renders one figure at the host's current width. Called on load and on
     resize; it rebuilds the svg rather than diffing it - a chart is ~1000
     nodes at most and a rebuild keeps the geometry honest at any width. */
  function draw(f, width) {
    var plot = f.plot;
    // The tooltip node outlives the svg, so it must be dismissed here: it
    // points at a crosshair in the svg being replaced, and its sample index
    // may not even exist in the new series.
    hideHover(f);
    // Drop the old svg but keep the tooltip node.
    if (f.svg && f.svg.parentNode) f.svg.parentNode.removeChild(f.svg);
    f.svg = null;
    var old = plot.querySelector('.cvchart-empty');
    if (old) plot.removeChild(old);

    var pts = f.samples || [];
    if (!pts.length) {
      var e = el('div', 'cvchart-empty');
      // A failed request and a genuinely empty window are different facts and
      // must not read the same: "no samples" would quietly present a 500 as
      // "this sensor recorded nothing".
      e.textContent = f.failed ? 'History unavailable' : 'No samples in this range';
      plot.appendChild(e);
      f.last.textContent = '--';
      f.geom = null;
      return;
    }

    var narrow = width < 340;
    var W = Math.max(160, Math.round(width));
    var H = narrow ? 112 : 148;
    var padL = narrow ? 30 : 38, padR = 10, padT = 8;
    var padB = 16; // the x-axis band lives INSIDE the svg height, never clipped
    var pw = Math.max(20, W - padL - padR);
    var ph = Math.max(20, H - padT - padB);

    var lo = Infinity, hi = -Infinity, i;
    for (i = 0; i < pts.length; i++) {
      if (pts[i].value < lo) lo = pts[i].value;
      if (pts[i].value > hi) hi = pts[i].value;
    }
    var ys = yScale(lo, hi, narrow ? 2 : 4);
    var dec = decimalsFor(ys.step);
    var xMin = f.xMin, xMax = f.xMax;
    if (!(xMax > xMin)) { xMin -= 30000; xMax += 30000; }

    var X = function (ts) { return padL + (ts - xMin) / (xMax - xMin) * pw; };
    var Y = function (v) { return padT + (ys.max - v) / (ys.max - ys.min) * ph; };

    var svg = svgEl('svg', { width: '100%', height: String(H), viewBox: '0 0 ' + W + ' ' + H, preserveAspectRatio: 'none', tabindex: '0', role: 'img' });
    // preserveAspectRatio=none would stretch text if the box were ever scaled;
    // the viewBox matches the rendered pixel width exactly, so it never is.

    var defs = svgEl('defs', {});
    var grad = svgEl('linearGradient', { id: f.uid + '-g', x1: '0', y1: '0', x2: '0', y2: '1' });
    // The stops carry their colour via CLASSES (see the css): var() resolves
    // in a css property value but never in a presentation attribute, so
    // stop-color="var(--x)" would silently paint nothing.
    grad.appendChild(svgEl('stop', { offset: '0', class: 'cvchart-grad-top' }));
    grad.appendChild(svgEl('stop', { offset: '1', class: 'cvchart-grad-bottom' }));
    defs.appendChild(grad);
    svg.appendChild(defs);

    // Gridlines: solid hairlines one step off the surface, never dashed.
    for (i = 0; i < ys.ticks.length; i++) {
      var yy = Math.round(Y(ys.ticks[i])) + 0.5;
      svg.appendChild(svgEl('line', { class: 'cvchart-gridline', x1: padL, y1: yy, x2: padL + pw, y2: yy }));
      var tl = svgEl('text', { class: 'cvchart-tick', x: padL - 5, y: yy + 3, 'text-anchor': 'end' });
      tl.textContent = fmtNum(ys.ticks[i], dec);
      svg.appendChild(tl);
    }

    // x ticks: as many as fit without colliding, at whole positions.
    var span = xMax - xMin;
    var fmt = tickFmt(span);
    // At the floor this is 2 labels (the ends, anchored inward), which cannot
    // collide with each other - better than a third label overlapping both.
    var nx = Math.max(1, Math.min(6, Math.floor(pw / tickBudget(fmt, narrow))));
    for (i = 0; i <= nx; i++) {
      var ts = xMin + span * (i / nx);
      var xx = X(ts);
      var anchor = i === 0 ? 'start' : i === nx ? 'end' : 'middle';
      var xt = svgEl('text', { class: 'cvchart-tick', x: Math.round(xx), y: H - 4, 'text-anchor': anchor });
      xt.textContent = fmtTime(ts, fmt);
      svg.appendChild(xt);
    }
    svg.appendChild(svgEl('line', { class: 'cvchart-axis', x1: padL, y1: Math.round(padT + ph) + 0.5, x2: padL + pw, y2: Math.round(padT + ph) + 0.5 }));

    // area + line
    var dLine = '', dArea = '';
    for (i = 0; i < pts.length; i++) {
      var px = X(pts[i].ts), py = Y(pts[i].value);
      dLine += (i ? 'L' : 'M') + px.toFixed(1) + ' ' + py.toFixed(1);
    }
    dArea = dLine + 'L' + X(pts[pts.length - 1].ts).toFixed(1) + ' ' + (padT + ph).toFixed(1) +
      'L' + X(pts[0].ts).toFixed(1) + ' ' + (padT + ph).toFixed(1) + 'Z';
    svg.appendChild(svgEl('path', { class: 'cvchart-area', d: dArea, fill: 'url(#' + f.uid + '-g)' }));
    svg.appendChild(svgEl('path', { class: 'cvchart-line', d: dLine }));

    // The endpoint marker + its always-visible value: the one direct label.
    var lastPt = pts[pts.length - 1];
    svg.appendChild(svgEl('circle', { class: 'cvchart-end', cx: X(lastPt.ts).toFixed(1), cy: Y(lastPt.value).toFixed(1), r: '4' }));
    f.last.textContent = fmtNum(lastPt.value, valDec(lastPt.value, f.kind)) + (f.unit ? ' ' + f.unit : '');

    var cross = svgEl('line', { class: 'cvchart-cross', x1: 0, y1: padT, x2: 0, y2: padT + ph, visibility: 'hidden' });
    svg.appendChild(cross);
    var dot = svgEl('circle', { class: 'cvchart-end', r: '4', visibility: 'hidden' });
    svg.appendChild(dot);

    // One transparent overlay is the hit target for the whole plot: the
    // pointer only has to be CLOSEST in x, never on the 2px line.
    var hit = svgEl('rect', { class: 'cvchart-hit', x: padL, y: padT, width: pw, height: ph });
    svg.appendChild(hit);

    svg.setAttribute('aria-label',
      f.label + ' over ' + fmtTime(xMin, TIP_FMT) + ' to ' + fmtTime(xMax, TIP_FMT) +
      '. Latest ' + fmtNum(lastPt.value, valDec(lastPt.value, f.kind)) + (f.unit ? ' ' + f.unit : '') +
      ', minimum ' + fmtNum(lo, valDec(lo, f.kind)) + ', maximum ' + fmtNum(hi, valDec(hi, f.kind)) + '.');

    plot.appendChild(svg);
    f.svg = svg;
    f.geom = { X: X, Y: Y, padL: padL, padT: padT, pw: pw, ph: ph, dec: dec, cross: cross, dot: dot, W: W };
    wireHover(f);
  }

  /* nearestIdx binary-searches the sample nearest a timestamp. */
  function nearestIdx(pts, ts) {
    var lo = 0, hi = pts.length - 1;
    if (ts <= pts[0].ts) return 0;
    if (ts >= pts[hi].ts) return hi;
    while (hi - lo > 1) {
      var mid = (lo + hi) >> 1;
      if (pts[mid].ts < ts) lo = mid; else hi = mid;
    }
    return (ts - pts[lo].ts <= pts[hi].ts - ts) ? lo : hi;
  }

  function showAt(f, idx) {
    var g = f.geom, pts = f.samples;
    if (!g || !pts || !pts.length) return;
    idx = Math.max(0, Math.min(pts.length - 1, idx));
    f.hover = idx;
    var p = pts[idx], x = g.X(p.ts), y = g.Y(p.value);
    g.cross.setAttribute('x1', x); g.cross.setAttribute('x2', x);
    g.cross.setAttribute('visibility', 'visible');
    g.dot.setAttribute('cx', x); g.dot.setAttribute('cy', y);
    g.dot.setAttribute('visibility', 'visible');

    var tip = f.tip;
    tip.textContent = '';
    var v = el('span', 'cvchart-tip-v');
    // Value leads, label follows: the reader already has the series and wants
    // the number.
    v.textContent = fmtNum(p.value, valDec(p.value, f.kind)) + (f.unit ? ' ' + f.unit : '');
    tip.appendChild(v);
    var t = el('span', 'cvchart-tip-t');
    t.textContent = fmtTime(p.ts, TIP_FMT);
    tip.appendChild(t);
    if (p.n > 1) {
      var n = el('span', 'cvchart-tip-n');
      n.textContent = 'mean of ' + p.n + ' readings';
      tip.appendChild(n);
    }
    tip.hidden = false;
    // Clamp inside the plot so the bubble never hangs off the panel edge.
    var tw = tip.offsetWidth || 90;
    var half = tw / 2;
    var scale = f.plot.clientWidth / (g.W || 1);
    var px = x * scale;
    tip.style.left = Math.max(half, Math.min(f.plot.clientWidth - half, px)) + 'px';
    tip.style.top = Math.max(0, y - 8) + 'px';
  }

  function hideHover(f) {
    if (f.geom) {
      f.geom.cross.setAttribute('visibility', 'hidden');
      f.geom.dot.setAttribute('visibility', 'hidden');
    }
    f.tip.hidden = true;
    f.hover = -1;
  }

  function wireHover(f) {
    var svg = f.svg;
    function tsAt(clientX) {
      var r = svg.getBoundingClientRect();
      var g = f.geom;
      var scale = (g.W || 1) / (r.width || 1);
      var vx = (clientX - r.left) * scale;
      var frac = (vx - g.padL) / g.pw;
      return f.xMin + frac * (f.xMax - f.xMin);
    }
    svg.addEventListener('pointermove', function (ev) {
      if (!f.samples || !f.samples.length || !f.geom) return;
      showAt(f, nearestIdx(f.samples, tsAt(ev.clientX)));
    });
    svg.addEventListener('pointerleave', function () { hideHover(f); });
    // Keyboard gets exactly what hover gets - the tooltip may enhance, never gate.
    svg.addEventListener('focus', function () {
      if (f.samples && f.samples.length) showAt(f, f.hover < 0 ? f.samples.length - 1 : f.hover);
    });
    svg.addEventListener('blur', function () { hideHover(f); });
    svg.addEventListener('keydown', function (ev) {
      if (!f.samples || !f.samples.length) return;
      var step = ev.shiftKey ? 10 : 1, cur = f.hover < 0 ? f.samples.length - 1 : f.hover;
      if (ev.key === 'ArrowLeft') { showAt(f, cur - step); ev.preventDefault(); }
      else if (ev.key === 'ArrowRight') { showAt(f, cur + step); ev.preventDefault(); }
      else if (ev.key === 'Home') { showAt(f, 0); ev.preventDefault(); }
      else if (ev.key === 'End') { showAt(f, f.samples.length - 1); ev.preventDefault(); }
      else if (ev.key === 'Escape' && f.hover >= 0) {
        // Both hosts close their whole panel on a document-level Escape (the
        // Device Center slide-out, the editor's inspector). Dismissing a
        // tooltip must not also close the panel - but Escape with no tooltip
        // open must still reach them, so only swallow it when there is
        // something of ours to dismiss.
        hideHover(f);
        ev.stopPropagation();
      }
    });
  }

  /* ----------------------------------------------------------------- mount */

  function mount(host, opts) {
    opts = opts || {};
    var device = opts.device;
    if (!host || !device) return { destroy: function () {} };

    var root = el('div', 'cvchart');
    var head = el('div', 'cvchart-head');
    if (opts.title) {
      var hl = el('span', 'cvchart-headline');
      hl.textContent = opts.title;
      head.appendChild(hl);
    }
    head.appendChild(el('span', 'cvchart-spacer'));

    // One filter row, above every chart it scopes - never one per chart.
    var seg = el('span', 'cvchart-seg');
    seg.setAttribute('role', 'radiogroup');
    seg.setAttribute('aria-label', 'Time range');
    var group = 'cvr' + (++uidSeq);
    var range = opts.range || '1d';
    RANGES.forEach(function (r) {
      var lab = el('label');
      var inp = el('input');
      inp.type = 'radio'; inp.name = group; inp.value = r.key;
      if (r.key === range) inp.checked = true;
      inp.addEventListener('change', function () { if (inp.checked) { range = r.key; load(); } });
      var sp = el('span');
      sp.textContent = r.label;
      lab.appendChild(inp); lab.appendChild(sp);
      seg.appendChild(lab);
    });
    head.appendChild(seg);
    root.appendChild(head);

    var body = el('div', 'cvchart-body');
    root.appendChild(body);
    host.appendChild(root);

    var figs = [];
    var abort = null;
    var gen = 0;
    var dead = false;
    var ro = null;

    function note(text) {
      body.textContent = '';
      var n = el('div', 'cvchart-note');
      n.textContent = text;
      body.appendChild(n);
    }

    function widthNow() {
      return body.clientWidth || host.clientWidth || 320;
    }

    function redrawAll() {
      var w = widthNow();
      figs.forEach(function (f) { if (f.samples) draw(f, w); });
    }

    function load() {
      if (dead || !figs.length) return;
      var my = ++gen;
      if (abort) abort.abort();
      abort = new AbortController();
      var sig = abort.signal;
      body.classList.add('is-loading');

      var now = Date.now();
      var r = RANGES.filter(function (x) { return x.key === range; })[0] || RANGES[0];
      var from = r.ms ? now - r.ms : 0;
      // Never ask for more points than the plot has pixels; the API downsamples
      // server-side with a weighted mean, so a month costs the same as an hour.
      var w = widthNow();
      var points = Math.max(60, Math.min(1000, Math.round(w)));

      var jobs = figs.map(function (f) {
        var url = '/a/devices/sensors/history?device=' + encodeURIComponent(device) +
          '&metric=' + encodeURIComponent(f.metric) +
          '&from=' + from + '&to=' + now + '&points=' + points;
        return fetchJSON(url, sig).then(function (d) {
          var out = (d && d.samples) || [];
          // The duty fraction becomes the percentage its unit already claims.
          if (f.kind === 'bool') {
            out = out.map(function (s) { return { ts: s.ts, value: s.value * 100, n: s.n }; });
          }
          f.samples = out;
          f.failed = false;
          if (r.ms) { f.xMin = from; f.xMax = now; }
          else if (out.length) { f.xMin = out[0].ts; f.xMax = out[out.length - 1].ts; }
          else { f.xMin = now - DAY; f.xMax = now; }
          return true;
        }).catch(function (err) {
          if (err && err.name === 'AbortError') return false;
          // Report the failure as a failure - see draw().
          f.samples = [];
          f.failed = true;
          return true;
        });
      });

      Promise.all(jobs).then(function () {
        // A slow response must never paint over a newer one (the user switched
        // range, or opened another device).
        if (dead || my !== gen) return;
        body.classList.remove('is-loading');
        redrawAll();
      });
    }

    fetchJSON('/a/devices/sensors/metrics?device=' + encodeURIComponent(device), null)
      .then(function (d) {
        if (dead) return;
        var all = (d && d.metrics) || [];
        var rec = all.filter(function (m) { return m.recorded; });
        if (!rec.length) {
          var pending = all.map(function (m) { return m.label || m.metric; });
          note(pending.length
            ? 'No history recorded yet. Recording starts automatically; the first averaged samples of ' +
              pending.join(', ') + ' will appear here.'
            : 'No history recorded for this device.');
          return;
        }
        body.textContent = '';
        rec.forEach(function (m) {
          var f = makeFigure(m);
          figs.push(f);
          body.appendChild(f.el);
        });
        load();
        if (window.ResizeObserver) {
          var raf = 0, lastW = widthNow();
          ro = new ResizeObserver(function () {
            var w = widthNow();
            if (w === lastW) return; // height-only changes must not redraw
            lastW = w;
            if (raf) cancelAnimationFrame(raf);
            raf = requestAnimationFrame(function () { raf = 0; if (!dead) redrawAll(); });
          });
          ro.observe(body);
        }
      })
      .catch(function () {
        if (!dead) note('History is unavailable right now.');
      });

    return {
      destroy: function () {
        dead = true;
        if (abort) abort.abort();
        if (ro) ro.disconnect();
        if (root.parentNode) root.parentNode.removeChild(root);
        figs = [];
      },
      reload: load
    };
  }

  window.CarvilonSensorChart = { mount: mount };
})();
