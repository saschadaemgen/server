// Saison 14-03: mieter mode-switcher runtime.
//
// The .stream slot in intercom-idle.html now hosts four
// .mode-layer children:
//
//   data-mode="screensaver" - clock + date + weather, default
//   data-mode="livestream"  - MJPEG <img>
//   data-mode="settings"    - inline form mirroring /webviewer/settings
//   data-mode="history"     - inline list backed by /webviewer/history.json
//
// All four sit absolute inside #idle-container. Only one carries
// .mode-active at a time; the others wait at translateY(100%)
// below the visible slot. switchMode() applies the slide-up
// transition (CSS in templates/viewer/home.html).
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
  // Mode switcher. switchMode applies .mode-active to the target
  // layer and removes it from every other layer. CSS handles the
  // slide-up transform; we just toggle the class.
  var currentMode = defaultMode;
  function switchMode(target) {
    if (!layers[target]) return;
    if (target === currentMode) {
      // Already on this mode; clicking the trigger again is a
      // user-intent to leave, so flip back to default if we're
      // looking at the settings or history mode.
      if (target === 'settings' || target === 'history') {
        target = defaultMode;
      } else {
        // Screensaver/livestream toggle stays on the explicit
        // tap-toggle path; calling switchMode with the same id is
        // a no-op here.
        return;
      }
    }
    Object.keys(layers).forEach(function (name) {
      if (name === target) {
        layers[name].classList.add('mode-active');
      } else {
        layers[name].classList.remove('mode-active');
      }
    });
    currentMode = target;
    if (target === 'history') {
      loadHistory();
    }
    resetAutoTimer();
  }

  // -------------------------------------------------------------
  // Auto-screensaver timer. Resets on any interaction inside the
  // container and on the explicit trigger buttons. Only armed if
  // the default mode is the screensaver AND the persisted value
  // is positive; otherwise the timer is disabled completely.
  var autoTimerHandle = null;
  function resetAutoTimer() {
    if (autoTimerHandle) {
      clearTimeout(autoTimerHandle);
      autoTimerHandle = null;
    }
    if (autoSeconds <= 0) return;
    if (defaultMode !== 'screensaver') return;
    if (currentMode === 'screensaver') return;
    autoTimerHandle = setTimeout(function () {
      switchMode('screensaver');
    }, autoSeconds * 1000);
  }

  // -------------------------------------------------------------
  // Tap toggle for the idle container. Limited to clicks on the
  // screensaver/livestream layers; clicks on settings/history
  // inputs, buttons, or the close-X are explicit and have their
  // own handlers below.
  container.addEventListener('click', function (e) {
    // Defence in depth: never swallow a click that landed on a
    // button, link, or form control - those have their own
    // semantics.
    if (e.target.closest('a, button, input, select, textarea, label')) {
      resetAutoTimer();
      return;
    }
    if (currentMode === 'screensaver' && layers.livestream) {
      switchMode('livestream');
    } else if (currentMode === 'livestream' && layers.screensaver) {
      switchMode('screensaver');
    }
  });

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
