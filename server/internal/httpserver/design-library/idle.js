// Saison 14-03 + 14-03-FIX01: mieter mode-switcher runtime.
//
// The .stream slot in intercom-idle.html hosts four .mode-layer
// children:
//
//   data-mode="screensaver" - clock + date + weather, default
//   data-mode="livestream"  - MJPEG <img>
//   data-mode="settings"    - inline form mirroring /webviewer/settings
//   data-mode="history"     - inline list backed by /webviewer/history.json
//
// All four sit absolute inside #idle-container. Their visible
// position comes from four modifier classes managed by setMode:
//
//   .below   parked under the slot   (translateY 100%)
//   .above   parked over the slot    (translateY -100%)
//   .active  showing                  (translateY 0)
//   .hidden  removed from interaction (visibility hidden)
//
// On every transition setMode flips the source from .active to
// .above (or .below for return-to-home) while flipping the target
// from .below (or .above) to .active, both in the same frame.
// Because each layer has its own transition on transform, the two
// slide in parallel - the source pushes the target through.
//
// Trigger surface:
//
//   - Tap on the screensaver  -> livestream (toggle)
//   - Tap on the livestream   -> screensaver (toggle)
//   - Topbar gear button      -> settings
//   - Action-bar history btn  -> history
//   - mode-close (X) buttons  -> back to default mode
//   - Auto-screensaver timer  -> back to default mode after N
//     seconds of inactivity (only if default is screensaver and
//     the persisted timer is > 0)
//
// FIX 1b: the livestream <img> ships with an empty src and a
// data-stream-src attribute. We fill the src after
// DOMContentLoaded so the multipart/x-mixed-replace response
// never blocks the initial paint of the home page.
//
// All clicks are scoped to the idle container or the explicit
// trigger buttons; nothing in here delegates the entire document,
// so the bell-overlay, unlock-door, mic, and logout buttons keep
// their own handlers.
(function () {
  var container = document.getElementById('idle-container');
  if (!container) {
    // Legacy page or unit test without the idle container.
    return;
  }

  var defaultMode = container.getAttribute('data-default-mode') || 'screensaver';
  if (defaultMode !== 'livestream') defaultMode = 'screensaver';

  var autoSeconds = parseInt(container.getAttribute('data-auto-screensaver-seconds') || '0', 10);
  if (!isFinite(autoSeconds) || autoSeconds < 0) autoSeconds = 0;

  var layers = {};
  var nodes = container.querySelectorAll('.mode-layer');
  for (var i = 0; i < nodes.length; i++) {
    var name = nodes[i].getAttribute('data-mode');
    if (name) layers[name] = nodes[i];
  }

  var clockEl = container.querySelector('.screensaver-clock');
  var dateEl = container.querySelector('.screensaver-date');
  var weatherEl = container.querySelector('.screensaver-weather');
  var tempEl = container.querySelector('.screensaver-temp');
  var descEl = container.querySelector('.screensaver-desc');
  var iconEl = container.querySelector('.screensaver-weather-icon');

  // -------------------------------------------------------------
  // Clock + date ticker. Always running so the screensaver stays
  // current regardless of which mode is on top.
  function tickClock() {
    if (!clockEl && !dateEl) return;
    var now = new Date();
    if (clockEl) {
      clockEl.textContent = now.toLocaleTimeString('de-DE', {
        hour: '2-digit', minute: '2-digit',
      });
    }
    if (dateEl) {
      dateEl.textContent = now.toLocaleDateString('de-DE', {
        weekday: 'long', day: 'numeric',
        month: 'short', year: 'numeric',
      });
    }
  }
  tickClock();
  setInterval(tickClock, 1000);

  // -------------------------------------------------------------
  // Weather refresh. Identical to S14-01b: 15-min cadence, hide
  // the weather block on backend errors.
  function showWeather(snap) {
    if (!weatherEl) return;
    weatherEl.style.display = '';
    if (tempEl && typeof snap.temp_c === 'number') {
      tempEl.textContent = Math.round(snap.temp_c) + '°C';
    }
    if (descEl && snap.description) {
      descEl.textContent = snap.description;
    }
    if (iconEl && snap.icon) {
      iconEl.setAttribute('data-icon', snap.icon);
    }
  }
  function hideWeather() {
    if (weatherEl) weatherEl.style.display = 'none';
  }
  function refreshWeather() {
    if (!weatherEl) return;
    fetch('/webviewer/weather', {
      headers: { 'Accept': 'application/json' },
      credentials: 'same-origin',
    })
      .then(function (r) {
        if (!r.ok) { hideWeather(); return null; }
        return r.json();
      })
      .then(function (snap) {
        if (!snap || typeof snap.temp_c !== 'number') {
          hideWeather();
          return;
        }
        showWeather(snap);
      })
      .catch(function () { hideWeather(); });
  }
  refreshWeather();
  setInterval(refreshWeather, 15 * 60 * 1000);

  // -------------------------------------------------------------
  // Mode switcher (S14-03-FIX01 push animation). Both layers travel
  // parallel via a CSS transition on transform: the source moves
  // from .active to .above (or .below for the return path), and
  // the target moves from .below (or .above) to .active in the
  // same frame. After the 400ms transition we mark the now-hidden
  // source with .hidden so it cannot intercept clicks.
  //
  // Direction rule: the screensaver is the conceptual "home" and
  // sits above the parked modes. Returning to it slides DOWN
  // (source leaves through the bottom, screensaver enters from
  // above); opening anything else slides UP (source leaves
  // through the top, target enters from below).
  //
  // An animation lock (`animating`) coalesces double-clicks so a
  // second trigger before the transition finishes is dropped.
  var activeMode = defaultMode;
  var animating = false;
  var ANIM_MS = 420; // 400ms transition + 20ms safety margin
  function setMode(target) {
    if (animating) return;
    if (!layers[target]) return;
    if (target === activeMode) {
      // Same-mode trigger: settings / history close back to the
      // user's default; screensaver / livestream are toggled by
      // the dedicated tap handler, so re-triggering them here is
      // a no-op.
      if (target === 'settings' || target === 'history') {
        target = defaultMode;
        if (target === activeMode) return;
      } else {
        return;
      }
    }
    var currentEl = layers[activeMode];
    var targetEl = layers[target];
    if (!currentEl || !targetEl) return;
    animating = true;

    var isReturnToHome = (target === 'screensaver');

    // Park the target on the opposite side of the slot and make it
    // visible BEFORE the transition kicks in. The void offsetHeight
    // expression forces the browser to flush the "parked" position
    // so the next frame's class swap actually animates instead of
    // teleporting.
    targetEl.classList.remove('above', 'below', 'active', 'hidden');
    targetEl.classList.add(isReturnToHome ? 'above' : 'below');
    void targetEl.offsetHeight;

    requestAnimationFrame(function () {
      currentEl.classList.remove('active');
      currentEl.classList.add(isReturnToHome ? 'below' : 'above');
      targetEl.classList.remove('above', 'below');
      targetEl.classList.add('active');
    });

    // Kick off the history fetch in parallel with the slide so the
    // payload is on its way before the layer is visible.
    if (target === 'history') {
      loadHistory();
    }

    setTimeout(function () {
      currentEl.classList.add('hidden');
      activeMode = target;
      animating = false;
      resetAutoTimer();
    }, ANIM_MS);
  }
  // Back-compat alias: earlier S14-03 code called switchMode.
  var switchMode = setMode;

  // -------------------------------------------------------------
  // Auto-screensaver timer (S14-03-FIX02 Sub-1b: "Variante B" -
  // timer runs in ANY non-screensaver mode when both prerequisites
  // hold: idle_default is screensaver AND auto_seconds > 0).
  //
  // Reset triggers (explicit):
  //   - any tap inside the device frame (container or topbar/
  //     action-bar - the document-level handler below)
  //   - mode switch (setMode calls this at the end of its
  //     transition)
  //   - doorbell event (home.html SSE handler calls
  //     window.unifixIdle.resetAutoTimer when a ring arrives)
  //
  // NOT a reset (the timer keeps ticking):
  //   - stream-frame arrivals (browser-internal, no JS event)
  //   - SSE heartbeat
  //   - weather refresh
  //
  // The two early-returns are by design:
  //   - autoSeconds == 0: feature disabled; don't waste a timer
  //   - defaultMode != 'screensaver': spec says the auto-timer
  //     only exists to bring the user BACK to the screensaver,
  //     which makes no sense when livestream is the persisted
  //     default
  //   - activeMode == 'screensaver': nowhere to fall back to
  var autoTimerHandle = null;
  function resetAutoTimer() {
    if (autoTimerHandle) {
      clearTimeout(autoTimerHandle);
      autoTimerHandle = null;
    }
    if (autoSeconds <= 0) return;
    if (defaultMode !== 'screensaver') return;
    if (activeMode === 'screensaver') return;
    autoTimerHandle = setTimeout(function () {
      setMode('screensaver');
    }, autoSeconds * 1000);
  }

  // -------------------------------------------------------------
  // Tap toggle for the idle container. Limited to clicks on the
  // screensaver/livestream layers; clicks on settings/history
  // inputs, buttons, or the close-X are explicit and have their
  // own handlers below.
  container.addEventListener('click', function (e) {
    // S14-03-FIX02: every tap inside the container resets the
    // timer, regardless of whether it triggers a mode switch
    // (taps on history rows, settings form whitespace, etc., all
    // count as user activity).
    resetAutoTimer();
    // Defence in depth: never swallow a click that landed on a
    // button, link, or form control - those have their own
    // semantics.
    if (e.target.closest('a, button, input, select, textarea, label')) {
      return;
    }
    if (activeMode === 'screensaver' && layers.livestream) {
      setMode('livestream');
    } else if (activeMode === 'livestream' && layers.screensaver) {
      setMode('screensaver');
    }
  });

  // S14-03-FIX02: any click anywhere in the device frame counts
  // as user activity (topbar gear, action-bar mic/unlock, even
  // accidental misses). Cheaper than tracking the .stage element
  // and bubbles up through the existing handlers.
  document.addEventListener('click', resetAutoTimer);

  // Expose a tiny hook so the doorbell SSE handler (in home.html,
  // separate <script>) can reset the timer when a ring arrives.
  window.unifixIdle = window.unifixIdle || {};
  window.unifixIdle.resetAutoTimer = resetAutoTimer;

  // -------------------------------------------------------------
  // Trigger buttons: gear (settings), history-action (history),
  // mode-close (back to default).
  document.addEventListener('click', function (e) {
    var settingsBtn = e.target.closest('[data-action="open-settings"]');
    if (settingsBtn) {
      e.preventDefault();
      switchMode('settings');
      return;
    }
    var historyBtn = e.target.closest('[data-action="open-history"]');
    if (historyBtn) {
      e.preventDefault();
      switchMode('history');
      return;
    }
    var closeBtn = e.target.closest('[data-action="mode-close"]');
    if (closeBtn) {
      e.preventDefault();
      switchMode(defaultMode);
      return;
    }
  });

  // -------------------------------------------------------------
  // Inline settings form: AJAX submit with Accept: application/json.
  // The server's JSON branch returns {"ok":true, ...} so we update
  // local runtime state without a page reload and bounce back to
  // the default mode.
  var settingsForm = container.querySelector('[data-form="settings"]');
  var settingsStatus = container.querySelector('[data-bind="settings-status"]');
  if (settingsForm) {
    settingsForm.addEventListener('submit', function (e) {
      e.preventDefault();
      var fd = new FormData(settingsForm);
      if (settingsStatus) settingsStatus.textContent = 'speichern ...';
      fetch(settingsForm.getAttribute('action') || '/webviewer/settings', {
        method: 'POST',
        body: fd,
        credentials: 'same-origin',
        headers: { 'Accept': 'application/json' },
      })
        .then(function (r) {
          if (!r.ok) throw new Error('settings http ' + r.status);
          return r.json();
        })
        .then(function (resp) {
          if (settingsStatus) settingsStatus.textContent = 'gespeichert';
          if (resp && typeof resp.idle_view_mode === 'string') {
            defaultMode = (resp.idle_view_mode === 'livestream') ? 'livestream' : 'screensaver';
            container.setAttribute('data-default-mode', defaultMode);
          }
          if (resp && typeof resp.auto_screensaver_seconds === 'number') {
            autoSeconds = resp.auto_screensaver_seconds;
            container.setAttribute('data-auto-screensaver-seconds', String(autoSeconds));
          }
          // Slide back to the (possibly new) default after a short
          // beat so the user can read the confirmation.
          setTimeout(function () {
            switchMode(defaultMode);
            if (settingsStatus) settingsStatus.textContent = '';
          }, 600);
        })
        .catch(function () {
          if (settingsStatus) settingsStatus.textContent = 'Fehler';
        });
    });
  }

  // -------------------------------------------------------------
  // Inline history: GET /webviewer/history.json on first open and
  // on every subsequent open so the list stays fresh. Server-side
  // mark-read happens asynchronously after the JSON ships, so the
  // "NEU" badges in this payload still render the first time.
  var historyList = container.querySelector('[data-bind="history-list"]');
  var historyEmpty = container.querySelector('[data-bind="history-empty"]');
  function loadHistory() {
    if (!historyList) return;
    if (historyEmpty) historyEmpty.textContent = 'Lade ...';
    fetch('/webviewer/history.json', {
      headers: { 'Accept': 'application/json' },
      credentials: 'same-origin',
    })
      .then(function (r) {
        if (!r.ok) throw new Error('history http ' + r.status);
        return r.json();
      })
      .then(function (resp) {
        renderHistory((resp && resp.events) || []);
      })
      .catch(function () {
        if (historyEmpty) {
          historyEmpty.textContent = 'Verlauf nicht erreichbar.';
        }
      });
  }
  function renderHistory(events) {
    if (!historyList) return;
    // Clear everything except the empty-state placeholder.
    while (historyList.firstChild) historyList.removeChild(historyList.firstChild);
    if (!events.length) {
      var empty = document.createElement('div');
      empty.className = 'mode-history-empty';
      empty.setAttribute('data-bind', 'history-empty');
      empty.textContent = 'Noch keine Klingel-Ereignisse.';
      historyList.appendChild(empty);
      historyEmpty = empty;
      return;
    }
    events.forEach(function (ev) {
      var row = document.createElement('div');
      row.className = 'mode-history-row';
      row.setAttribute('role', 'listitem');
      if (ev.unread) row.classList.add('is-unread');
      var where = document.createElement('span');
      where.className = 'mode-history-where';
      where.textContent = ev.door_name || ev.intercom_mac || 'Hauseingang';
      var when = document.createElement('span');
      when.className = 'mode-history-when';
      when.textContent = ev.when || '';
      row.appendChild(where);
      row.appendChild(when);
      if (ev.unread) {
        var badge = document.createElement('span');
        badge.className = 'mode-history-badge';
        badge.textContent = 'NEU';
        row.appendChild(badge);
      }
      historyList.appendChild(row);
    });
  }

  // -------------------------------------------------------------
  // Help-icon tooltips. Plain title-based hover plus a click-to-
  // pin-on-mobile pattern: tapping the (?) toggles a sibling
  // tooltip bubble next to the legend.
  document.addEventListener('click', function (e) {
    var help = e.target.closest('.mode-help');
    if (!help) {
      // Click outside any help bubble -> close any open ones.
      var open = document.querySelectorAll('.mode-help.is-open');
      for (var i = 0; i < open.length; i++) open[i].classList.remove('is-open');
      return;
    }
    e.preventDefault();
    e.stopPropagation();
    help.classList.toggle('is-open');
  });

  // -------------------------------------------------------------
  // FIX 1b: defer the MJPEG src to DOMContentLoaded so the
  // multipart response cannot block the initial paint.
  function hydrateLivestream() {
    var img = container.querySelector('[data-mode="livestream"] img[data-stream-src]');
    if (!img) return;
    var src = img.getAttribute('data-stream-src');
    if (src) img.setAttribute('src', src);
  }
  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', hydrateLivestream);
  } else {
    hydrateLivestream();
  }

  // -------------------------------------------------------------
  // Arm the timer once on startup so opening the page on
  // livestream-as-default + auto-timer leaves the user on
  // livestream (because defaultMode !== screensaver, the guard
  // in resetAutoTimer returns immediately).
  resetAutoTimer();
})();
