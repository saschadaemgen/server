/* ============================================================
 * carvilon · interactions.js
 * ------------------------------------------------------------
 * Minimal vanilla JavaScript for the carvilon library. No build,
 * no dependencies. Load with a normal <script src="interactions.js">.
 *
 * Public surface (all available on window.carvilon):
 *
 *   carvilon.openSheet(name)
 *   carvilon.closeSheet(name)
 *   carvilon.openModal(name)
 *   carvilon.closeModal(name)
 *   carvilon.openOverlay(name)     // e.g. "ringing"
 *   carvilon.closeOverlay(name)
 *   carvilon.setTheme("light" | "dark" | "system")
 *   carvilon.setDND(true | false)
 *
 *   carvilon.connectSSE(url, handlers)
 *     handlers = {
 *       onDoorbellStart(payload), onDoorbellCancel(payload),
 *       onHistoryUpdate(payload), onConnect(), onError(err)
 *     }
 *
 * Auto-wired declaratively via these data-action attributes:
 *
 *   data-action="open-sheet"     data-sheet-name="history"
 *   data-action="close-sheet"
 *   data-action="open-modal"     data-modal-name="magic-link"
 *   data-action="close-modal"
 *   data-action="open-overlay"   data-overlay-name="ringing"
 *   data-action="close-overlay"
 *   data-action="toggle-theme"
 *   data-action="toggle-dnd"
 *   data-action="copy-magic-link"
 *   data-action="open-history"   (alias of open-sheet history)
 *   data-action="open-settings"  (you wire your own router)
 *   data-action="ignore-call"    (closes the ringing overlay)
 * ============================================================ */

(function () {
  'use strict';

  // --------------------------------------------------------------
  // Theme  ·  dark / light / system
  // --------------------------------------------------------------

  var THEME_KEY = 'carvilon.theme';

  function setTheme(theme) {
    // theme: "dark" | "light" | "system"
    var resolved = theme;
    if (theme === 'system') {
      resolved = matchMedia('(prefers-color-scheme: light)').matches
                 ? 'light' : 'dark';
    }
    document.documentElement.setAttribute('data-theme', resolved);
    try { localStorage.setItem(THEME_KEY, theme); } catch (e) {}
  }

  function bootTheme() {
    var saved = null;
    try { saved = localStorage.getItem(THEME_KEY); } catch (e) {}
    setTheme(saved || 'dark');

    // If user picked "system", follow OS changes
    if (saved === 'system') {
      matchMedia('(prefers-color-scheme: light)')
        .addEventListener('change', function () { setTheme('system'); });
    }
  }

  function cycleTheme() {
    // Cycle: dark → light → system → dark
    var current = null;
    try { current = localStorage.getItem(THEME_KEY); } catch (e) {}
    var next = current === 'dark' ? 'light'
             : current === 'light' ? 'system'
             : 'dark';
    setTheme(next);
  }


  // --------------------------------------------------------------
  // Generic open/close helpers (sheet, modal, overlay)
  // --------------------------------------------------------------

  function applyOpen(selector, open) {
    var els = document.querySelectorAll(selector);
    for (var i = 0; i < els.length; i++) {
      els[i].classList.toggle('is-open', !!open);
    }
  }

  function openSheet(name) {
    // The sheet and its matching scrim both get is-open
    applyOpen('[data-sheet="' + name + '"]',       true);
    applyOpen('[data-sheet-scrim="' + name + '"]', true);
    document.body.dataset.activeSheet = name;
  }
  function closeSheet(name) {
    if (!name) {
      // close any sheet
      var sheets = document.querySelectorAll('[data-sheet]');
      for (var i = 0; i < sheets.length; i++) {
        sheets[i].classList.remove('is-open');
      }
      var scrims = document.querySelectorAll('[data-sheet-scrim]');
      for (var j = 0; j < scrims.length; j++) {
        scrims[j].classList.remove('is-open');
      }
      delete document.body.dataset.activeSheet;
      return;
    }
    applyOpen('[data-sheet="' + name + '"]',       false);
    applyOpen('[data-sheet-scrim="' + name + '"]', false);
    if (document.body.dataset.activeSheet === name) {
      delete document.body.dataset.activeSheet;
    }
  }

  function openModal(name)  { applyOpen('[data-modal="' + name + '"]',   true);  }
  function closeModal(name) {
    if (!name) {
      var modals = document.querySelectorAll('[data-modal]');
      for (var i = 0; i < modals.length; i++) modals[i].classList.remove('is-open');
      return;
    }
    applyOpen('[data-modal="' + name + '"]', false);
  }

  // Saison 15-01: WebRTC lifecycle hook for the ringing overlay.
  // openOverlay('ringing') -> webrtc.connect on the overlay's
  // <video data-webrtc-target="ringing">. closeOverlay('ringing')
  // -> disconnect. We only react to the ringing overlay; other
  // overlays (data-overlay attribute names) have no stream slot.
  function hookRingingWebRTC(name, open) {
    if (name !== 'ringing' || !window.carvilonWebRTC) return;
    if (open) {
      var video = document.querySelector('[data-overlay="ringing"] video[data-webrtc-target="ringing"]');
      if (video) window.carvilonWebRTC.connect(video);
    } else {
      window.carvilonWebRTC.disconnect();
    }
  }

  function openOverlay(name)  {
    applyOpen('[data-overlay="' + name + '"]', true);
    hookRingingWebRTC(name, true);
  }
  function closeOverlay(name) {
    applyOpen('[data-overlay="' + name + '"]', false);
    hookRingingWebRTC(name, false);
  }


  // --------------------------------------------------------------
  // DND toggle (does not POST; UI-only — your route handles POST)
  // --------------------------------------------------------------

  function setDND(active) {
    var btns = document.querySelectorAll('[data-action="toggle-dnd"]');
    for (var i = 0; i < btns.length; i++) {
      btns[i].classList.toggle('is-on', !!active);
      btns[i].setAttribute('aria-pressed', !!active);
    }
    var subs = document.querySelectorAll('.identity-sub');
    for (var j = 0; j < subs.length; j++) {
      subs[j].textContent = active ? 'Nicht stören' : 'Online';
    }
  }


  // --------------------------------------------------------------
  // Clock — refreshes elements with [data-bind="clock-time"] every
  // second. Page can omit this and pre-render server-side instead.
  // --------------------------------------------------------------

  function startClock() {
    var nodes = document.querySelectorAll('[data-bind="clock-time"]');
    if (!nodes.length) return;
    function tick() {
      var d = new Date();
      var hh = String(d.getHours()).padStart(2, '0');
      var mm = String(d.getMinutes()).padStart(2, '0');
      var ss = String(d.getSeconds()).padStart(2, '0');
      var t = hh + ':' + mm + ':' + ss;
      for (var i = 0; i < nodes.length; i++) nodes[i].textContent = t;
    }
    tick();
    setInterval(tick, 1000);
  }


  // --------------------------------------------------------------
  // Copy magic link
  // --------------------------------------------------------------

  function copyMagicLink(btn) {
    var modal = btn.closest('[data-modal="magic-link"]');
    var urlEl = modal && modal.querySelector('[data-magic-url]');
    if (!urlEl) return;
    var url = urlEl.textContent.trim();
    var label = btn.querySelector('[data-copy-label]');
    var done = function () {
      btn.classList.add('is-copied');
      if (label) label.textContent = 'Kopiert';
      setTimeout(function () {
        btn.classList.remove('is-copied');
        if (label) label.textContent = 'Kopieren';
      }, 1600);
    };
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(url).then(done, function () {});
    } else {
      // Fallback: select + execCommand
      var range = document.createRange();
      range.selectNode(urlEl);
      var sel = window.getSelection();
      sel.removeAllRanges();
      sel.addRange(range);
      try { document.execCommand('copy'); done(); } catch (e) {}
      sel.removeAllRanges();
    }
  }


  // --------------------------------------------------------------
  // Server-Sent Events
  // ------------------------------------------------------------
  // Stub wiring for doorbell events. Connect like:
  //
  //   carvilon.connectSSE("/intercom/stream", {
  //     onDoorbellStart:  (p) => carvilon.openOverlay("ringing"),
  //     onDoorbellCancel: ()  => carvilon.closeOverlay("ringing"),
  //     onHistoryUpdate:  (p) => location.reload(),
  //   });
  //
  // Server contract (per line, JSON):
  //   event: doorbell_start
  //   data:  { door, ts }
  //
  //   event: doorbell_cancel
  //   data:  { door, ts }
  //
  //   event: history_update
  //   data:  { items: [...] }
  // --------------------------------------------------------------

  function connectSSE(url, handlers) {
    handlers = handlers || {};
    if (!window.EventSource) {
      handlers.onError && handlers.onError(new Error('EventSource unsupported'));
      return null;
    }
    var es = new EventSource(url);

    es.addEventListener('open',  function ()      { handlers.onConnect && handlers.onConnect(); });
    es.addEventListener('error', function (err)   { handlers.onError   && handlers.onError(err); });

    es.addEventListener('doorbell_start', function (e) {
      var p = parseJSON(e.data);
      handlers.onDoorbellStart && handlers.onDoorbellStart(p);
    });
    es.addEventListener('doorbell_cancel', function (e) {
      var p = parseJSON(e.data);
      handlers.onDoorbellCancel && handlers.onDoorbellCancel(p);
    });
    es.addEventListener('history_update', function (e) {
      var p = parseJSON(e.data);
      handlers.onHistoryUpdate && handlers.onHistoryUpdate(p);
    });

    return es;
  }

  function parseJSON(s) {
    try { return JSON.parse(s); } catch (e) { return s; }
  }


  // --------------------------------------------------------------
  // Declarative event wiring (capture phase, single listener)
  // --------------------------------------------------------------

  document.addEventListener('click', function (e) {
    var btn = e.target.closest('[data-action]');
    if (!btn) {
      // Tap on scrim closes its sheet
      var scrim = e.target.closest('[data-sheet-scrim]');
      if (scrim) {
        closeSheet(scrim.getAttribute('data-sheet-scrim'));
      }
      // Tap on modal scrim (outside .modal-glass) closes the modal
      var modal = e.target.closest('[data-modal]');
      if (modal && !e.target.closest('.modal-glass')) {
        modal.classList.remove('is-open');
      }
      return;
    }

    var action = btn.getAttribute('data-action');
    switch (action) {

      case 'open-sheet':
      case 'open-history':
        e.preventDefault();
        openSheet(btn.getAttribute('data-sheet-name') || 'history');
        break;

      case 'close-sheet':
        e.preventDefault();
        var sheet = btn.closest('[data-sheet]');
        closeSheet(sheet ? sheet.getAttribute('data-sheet') : null);
        break;

      case 'open-modal':
        e.preventDefault();
        openModal(btn.getAttribute('data-modal-name'));
        break;

      case 'close-modal':
        e.preventDefault();
        var m = btn.closest('[data-modal]');
        closeModal(m ? m.getAttribute('data-modal') : null);
        break;

      case 'open-overlay':
        e.preventDefault();
        openOverlay(btn.getAttribute('data-overlay-name'));
        break;

      case 'close-overlay':
      case 'ignore-call':
        e.preventDefault();
        var ov = btn.closest('[data-overlay]');
        closeOverlay(ov ? ov.getAttribute('data-overlay') : 'ringing');
        break;

      case 'toggle-theme':
        e.preventDefault();
        cycleTheme();
        break;

      case 'toggle-dnd':
        e.preventDefault();
        var isOn = btn.classList.contains('is-on');
        setDND(!isOn);
        // Notify server (stub — wire to your route)
        if (window.carvilon.onDNDChange) window.carvilon.onDNDChange(!isOn);
        break;

      case 'copy-magic-link':
        e.preventDefault();
        copyMagicLink(btn);
        break;

      // Other actions (open-cameras, open-settings, unlock-door,
      // accept-call, regenerate-magic-link, send-magic-link, etc.)
      // are left to the host application to wire — likely as form
      // submissions or fetch() calls in your Go-templated routes.

      default: /* let the browser handle it */ break;
    }
  });

  // Esc closes whatever is open (modal > sheet > overlay)
  document.addEventListener('keydown', function (e) {
    if (e.key !== 'Escape') return;
    var openModalEl = document.querySelector('[data-modal].is-open');
    if (openModalEl) { openModalEl.classList.remove('is-open'); return; }
    var openSheetEl = document.querySelector('[data-sheet].is-open');
    if (openSheetEl) { closeSheet(openSheetEl.getAttribute('data-sheet')); return; }
    var openOverlayEl = document.querySelector('[data-overlay].is-open');
    if (openOverlayEl) {
      var name = openOverlayEl.getAttribute('data-overlay');
      openOverlayEl.classList.remove('is-open');
      // Saison 15-01: keep the WebRTC lifecycle in sync when
      // ESC bypasses the closeOverlay helper.
      hookRingingWebRTC(name, false);
    }
  });

  // Theme-radio inside settings forms (segment buttons with data-theme)
  document.addEventListener('click', function (e) {
    var b = e.target.closest('[data-theme]');
    if (!b) return;
    var t = b.getAttribute('data-theme');
    setTheme(t);
    // Update segment visual state
    var segment = b.closest('.segment');
    if (segment) {
      var btns = segment.querySelectorAll('button');
      for (var i = 0; i < btns.length; i++) btns[i].classList.toggle('is-active', btns[i] === b);
    }
    var hidden = b.closest('form') && b.closest('form').querySelector('input[name="admin_theme"]');
    if (hidden) hidden.value = t;
  });

  // Toggle switches inside forms (sync the paired hidden input)
  document.addEventListener('click', function (e) {
    var t = e.target.closest('.toggle[data-toggle-name]');
    if (!t) return;
    var on = t.classList.toggle('is-on');
    t.setAttribute('aria-checked', on);
    var name = t.getAttribute('data-toggle-name');
    var form = t.closest('form');
    var hidden = form && form.querySelector('input[name="' + name + '"]');
    if (hidden) hidden.value = on ? 'true' : 'false';
  });

  // Slider — paint the fill via --p
  document.addEventListener('input', function (e) {
    if (!e.target.matches('.slider')) return;
    var input = e.target;
    var min = parseFloat(input.min || 0);
    var max = parseFloat(input.max || 100);
    var val = parseFloat(input.value);
    var pct = ((val - min) / (max - min)) * 100;
    input.style.setProperty('--p', pct + '%');
    var head = input.closest('.slider-row');
    var out = head && head.querySelector('[data-slider-output]');
    if (out) out.textContent = Math.round(val) + '%';
  });

  // Initialize slider fills on load
  document.addEventListener('DOMContentLoaded', function () {
    var sliders = document.querySelectorAll('.slider');
    for (var i = 0; i < sliders.length; i++) {
      sliders[i].dispatchEvent(new Event('input', { bubbles: true }));
    }
  });

  // Boot
  bootTheme();
  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', startClock);
  } else {
    startClock();
  }


  // --------------------------------------------------------------
  // Export public API
  // --------------------------------------------------------------

  window.carvilon = {
    setTheme:     setTheme,
    cycleTheme:   cycleTheme,
    openSheet:    openSheet,
    closeSheet:   closeSheet,
    openModal:    openModal,
    closeModal:   closeModal,
    openOverlay:  openOverlay,
    closeOverlay: closeOverlay,
    setDND:       setDND,
    connectSSE:   connectSSE,
  };

})();
