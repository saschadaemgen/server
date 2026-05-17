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

  // Saison 14-XX: drei persistierbare Idle-Defaults im Server-
  // Vokabular (screensaver | livestream | screen_off). Der
  // Web-Viewer hat aber nur zwei Render-Pfade - screen_off ist
  // ein ESP-Hardware-Konzept (Backlight aus). Wir mappen
  // screen_off hier auf screensaver, damit ein Cross-Device-
  // Switch am ESP (Mieter setzt am ESP screen_off, Web-Browser
  // zieht via config.changed nach) keinen leeren Container
  // rendert.
  var defaultMode = container.getAttribute('data-default-mode') || 'screensaver';
  if (defaultMode === 'screen_off') defaultMode = 'screensaver';
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
  // S14-03-FIX02 Sub-1d: Lucide-shaped SVG dictionary. The
  // screensaver previously rendered weather icons via CSS mask
  // (-webkit-mask: var(--icon-cloud)) but those tokens were
  // never defined, so the icon span fell back to a solid grey
  // background-color square. Now we inject the real SVG markup
  // directly into the span and let stroke=currentColor inherit
  // the screensaver's text color.
  //
  // Icon names match wmo_codes.go (Lucide name set, e.g. "cloud",
  // "sun", "cloud-rain"). Unknown names fall back to "cloud".
  var WEATHER_ICONS = {
    'sun': '<svg width="22" height="22" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="4"/><path d="M12 2v2"/><path d="M12 20v2"/><path d="m4.93 4.93 1.41 1.41"/><path d="m17.66 17.66 1.41 1.41"/><path d="M2 12h2"/><path d="M20 12h2"/><path d="m4.93 19.07 1.41-1.41"/><path d="m17.66 6.34 1.41-1.41"/></svg>',
    'cloud': '<svg width="22" height="22" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M17.5 19H9a7 7 0 1 1 6.71-9h1.79a4.5 4.5 0 1 1 0 9Z"/></svg>',
    'cloud-sun': '<svg width="22" height="22" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M12 2v2"/><path d="m4.93 4.93 1.41 1.41"/><path d="M20 12h2"/><path d="m19.07 4.93-1.41 1.41"/><path d="M15.947 12.65a4 4 0 0 0-5.925-4.128"/><path d="M13 22H7a5 5 0 1 1 4.9-6H13a3 3 0 0 1 0 6Z"/></svg>',
    'cloud-fog': '<svg width="22" height="22" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M16 17H7"/><path d="M17 21H9"/><path d="M17 13a5 5 0 1 0-9.9-1.1A4 4 0 1 0 5 19h12a4 4 0 0 0 0-8z"/></svg>',
    'cloud-drizzle': '<svg width="22" height="22" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M4 14.899A7 7 0 1 1 15.71 8h1.79a4.5 4.5 0 0 1 2.5 8.242"/><path d="M8 19v1"/><path d="M8 14v1"/><path d="M16 19v1"/><path d="M16 14v1"/><path d="M12 21v1"/><path d="M12 16v1"/></svg>',
    'cloud-rain': '<svg width="22" height="22" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M4 14.899A7 7 0 1 1 15.71 8h1.79a4.5 4.5 0 0 1 2.5 8.242"/><path d="M16 14v6"/><path d="M8 14v6"/><path d="M12 16v6"/></svg>',
    'cloud-rain-wind': '<svg width="22" height="22" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M4 14.899A7 7 0 1 1 15.71 8h1.79a4.5 4.5 0 0 1 2.5 8.242"/><path d="m9.2 22 3-7"/><path d="m9 13-3 7"/><path d="m17 13-3 7"/></svg>',
    'snowflake': '<svg width="22" height="22" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M2 12h20"/><path d="M12 2v20"/><path d="m20 16-4-4 4-4"/><path d="m4 8 4 4-4 4"/><path d="m16 4-4 4-4-4"/><path d="m8 20 4-4 4 4"/></svg>',
    'cloud-lightning': '<svg width="22" height="22" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M6 16.326A7 7 0 1 1 15.71 8h1.79a4.5 4.5 0 0 1 .5 8.973"/><path d="m13 12-3 5h4l-3 5"/></svg>'
  };
  function setWeatherIcon(name) {
    if (!iconEl) return;
    var svg = WEATHER_ICONS[name] || WEATHER_ICONS['cloud'];
    iconEl.innerHTML = svg;
    iconEl.setAttribute('data-icon', name || 'cloud');
  }

  function showWeather(snap) {
    if (!weatherEl) return;
    weatherEl.style.display = '';
    if (tempEl && typeof snap.temp_c === 'number') {
      tempEl.textContent = Math.round(snap.temp_c) + '°C';
    }
    if (descEl && snap.description) {
      descEl.textContent = snap.description;
    }
    if (snap.icon) {
      setWeatherIcon(snap.icon);
    }
  }

  // Hydrate the initial icon from the server-rendered data-icon
  // attribute so the screensaver shows a real SVG immediately,
  // before the first weather refresh response lands.
  if (iconEl && !iconEl.firstChild) {
    setWeatherIcon(iconEl.getAttribute('data-icon') || 'cloud');
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
  // Mode switcher (S14-03-FIX01 push animation + FIX02 Sub-1c
  // idle-default-aware direction). Both layers travel parallel
  // via a CSS transition on transform: the source moves from
  // .active to .above (or .below for the return path), and the
  // target moves from .below (or .above) to .active in the same
  // frame. After the 400ms transition we mark the now-hidden
  // source with .hidden so it cannot intercept clicks.
  //
  // Direction rule (FIX02): the IDLE mode (the mieter's persisted
  // default - screensaver OR livestream) is conceptually "above"
  // and parked modes sit "below". Returning to idle slides DOWN
  // (target enters from above, source leaves down); opening
  // anything else slides UP (target enters from below, source
  // leaves up). Pre-FIX02 the direction was hard-coded to
  // screensaver-as-home; with livestream-as-default the close
  // direction felt wrong.
  //
  // An animation lock (`animating`) coalesces double-clicks so a
  // second trigger before the transition finishes is dropped.
  var activeMode = defaultMode;
  var animating = false;
  var ANIM_MS = 420; // 400ms transition + 20ms safety margin

  // S14-03-FIX06: the cam-meta chip sits in the .stream wrapper
  // as a sibling of #idle-container (it is not a child of any
  // mode-layer, so opaque settings/history layers cannot hide
  // it on their own). updateStreamMetaVisibility toggles the
  // .is-hidden utility class on it based on the active mode.
  var streamMetaEl = container.parentElement
    ? container.parentElement.querySelector('.stream-meta')
    : null;
  function updateStreamMetaVisibility(mode) {
    if (!streamMetaEl) return;
    var hide = (mode === 'settings' || mode === 'history');
    streamMetaEl.classList.toggle('is-hidden', hide);
  }
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

    var isReturnToIdle = (target === defaultMode);

    // Park the target on the opposite side of the slot and make it
    // visible BEFORE the transition kicks in. The void offsetHeight
    // expression forces the browser to flush the "parked" position
    // so the next frame's class swap actually animates instead of
    // teleporting.
    targetEl.classList.remove('above', 'below', 'active', 'hidden');
    targetEl.classList.add(isReturnToIdle ? 'above' : 'below');
    void targetEl.offsetHeight;

    requestAnimationFrame(function () {
      currentEl.classList.remove('active');
      currentEl.classList.add(isReturnToIdle ? 'below' : 'above');
      targetEl.classList.remove('above', 'below');
      targetEl.classList.add('active');
    });

    // S14-03-FIX06: the cam-meta chip (`cam · DoorName` at
    // bottom-left of the slot) only belongs to the screensaver
    // and livestream modes. In settings/history it would
    // shine through the opaque mode-layer because it lives in
    // the .stream wrapper, outside #idle-container.
    updateStreamMetaVisibility(target);

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
  //     window.carvilonIdle.resetAutoTimer when a ring arrives)
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
  window.carvilonIdle = window.carvilonIdle || {};
  window.carvilonIdle.resetAutoTimer = resetAutoTimer;

  // -------------------------------------------------------------
  // Saison 14-03-FIX03 Sub-2: unread-doorbell badge runtime.
  //
  // Three event sources feed the badge:
  //   1. initial fetch /webviewer/unread-count on page load
  //   2. SSE doorbell_start (count +=1, set via the home-page SSE
  //      script which calls window.carvilonIdle.bumpUnread()
  //      because it has access to the EventSource we don't)
  //   3. history mode opening (we just fetched the list, the
  //      server marks rows read async, so we drop the badge to 0)
  //
  // The badge is positioned inside the screensaver layer so it
  // travels with screensaver-on-top / screensaver-below
  // transitions; no separate visibility logic needed.
  var badgeEl = container.querySelector('[data-bind="unread-badge"]');
  var badgeCountEl = container.querySelector('[data-bind="unread-count"]');
  var badgeIconEl = container.querySelector('.screensaver-badge-icon');
  // Lucide-style "bell-ring" icon inlined as a one-shot string;
  // no extra library load.
  var BELL_RING_SVG = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><path d="M5.5 17h13l-1.4-1.8c-.4-.5-.6-1.1-.6-1.8V10a4.5 4.5 0 0 0-9 0v3.4c0 .7-.2 1.3-.6 1.8L5.5 17Z"/><path d="M10 19.5a2 2 0 0 0 4 0"/><path d="M21 9c-.7-1.8-1.7-3.4-3-4.7"/><path d="M3 9c.7-1.8 1.7-3.4 3-4.7"/></svg>';
  if (badgeIconEl && !badgeIconEl.firstChild) {
    badgeIconEl.innerHTML = BELL_RING_SVG;
  }

  // S14-03-FIX04 Sub-2: the action-bar history button mirrors
  // the count via its own badge + radar pulse. Looked up once
  // here so updateUnreadBadge does not query the DOM on every
  // call.
  var historyBtnEl = document.querySelector('[data-action="open-history"]');
  var historyBtnBadgeEl = document.querySelector('[data-bind="history-button-badge"]');

  function formatBadgeCount(n) {
    return n > 99 ? '99+' : String(n);
  }

  var unreadCount = 0;
  function updateUnreadBadge(n) {
    unreadCount = Math.max(0, n | 0);
    // Screensaver chip.
    if (badgeEl) {
      badgeEl.setAttribute('data-count', String(unreadCount));
      if (unreadCount > 0) {
        badgeEl.hidden = false;
        if (badgeCountEl) badgeCountEl.textContent = String(unreadCount);
      } else {
        badgeEl.hidden = true;
        if (badgeCountEl) badgeCountEl.textContent = '0';
      }
    }
    // History button: pulse rings + count chip.
    if (historyBtnEl) {
      if (unreadCount > 0) {
        historyBtnEl.classList.add('has-unread');
      } else {
        historyBtnEl.classList.remove('has-unread');
      }
    }
    if (historyBtnBadgeEl) {
      if (unreadCount > 0) {
        historyBtnBadgeEl.hidden = false;
        historyBtnBadgeEl.textContent = formatBadgeCount(unreadCount);
      } else {
        historyBtnBadgeEl.hidden = true;
        historyBtnBadgeEl.textContent = '';
      }
    }
  }
  function bumpUnread() {
    updateUnreadBadge(unreadCount + 1);
  }

  function fetchUnreadCount() {
    if (!badgeEl) return;
    fetch('/webviewer/unread-count', {
      headers: { 'Accept': 'application/json' },
      credentials: 'same-origin',
    })
      .then(function (r) {
        if (!r.ok) throw new Error('unread-count http ' + r.status);
        return r.json();
      })
      .then(function (resp) {
        updateUnreadBadge((resp && resp.count) || 0);
      })
      .catch(function () { /* leave badge as-is on transient error */ });
  }
  // Initial hydrate after DOMContentLoaded so the badge is
  // correct before the user has interacted with anything.
  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', fetchUnreadCount);
  } else {
    fetchUnreadCount();
  }

  // Expose for home.html's doorbell SSE handler and for the
  // SSE unread_count listener it'll wire up.
  window.carvilonIdle.bumpUnread = bumpUnread;
  window.carvilonIdle.updateUnreadBadge = updateUnreadBadge;

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
  // S14-03-FIX02 Sub-1e: auto-save settings on every radio
  // change. Replaces the explicit Save button. Each change fires
  // one POST with the FULL form state so the server always sees
  // both fields; the JSON response patches local runtime state
  // (defaultMode, autoSeconds) and resetAutoTimer re-arms the
  // timer with the new value. No mode-switch on save - the user
  // stays in settings so they can adjust further options.
  var settingsForm = container.querySelector('[data-form="settings"]');
  var toastEl = container.querySelector('[data-bind="toast"]');
  var toastTimer = null;

  function showToast(message, kind, durationMs) {
    if (!toastEl) return;
    toastEl.textContent = message;
    toastEl.classList.remove('kind-error');
    if (kind === 'error') toastEl.classList.add('kind-error');
    toastEl.classList.add('is-visible');
    if (toastTimer) clearTimeout(toastTimer);
    toastTimer = setTimeout(function () {
      toastEl.classList.remove('is-visible');
    }, durationMs || 1500);
  }

  function saveSettings() {
    if (!settingsForm) return;
    // S14-03-FIX03 Sub-1a: build a urlencoded body explicitly.
    // The previous FIX02 code passed a FormData object directly,
    // which makes fetch use multipart/form-data. Go's
    // r.ParseForm only populates r.PostForm for the urlencoded
    // content-type; multipart bodies leave PostForm empty, so
    // BOTH fields were silently ignored on save.
    var body = new URLSearchParams();
    var inputs = settingsForm.querySelectorAll('input[type="radio"]:checked');
    for (var bi = 0; bi < inputs.length; bi++) {
      body.append(inputs[bi].name, inputs[bi].value);
    }
    fetch(settingsForm.getAttribute('action') || '/webviewer/settings', {
      method: 'POST',
      body: body.toString(),
      credentials: 'same-origin',
      headers: {
        'Accept': 'application/json',
        'Content-Type': 'application/x-www-form-urlencoded',
      },
    })
      .then(function (r) {
        if (!r.ok) throw new Error('settings http ' + r.status);
        return r.json();
      })
      .then(function (resp) {
        if (resp && typeof resp.idle_view_mode === 'string') {
          defaultMode = (resp.idle_view_mode === 'livestream') ? 'livestream' : 'screensaver';
          container.setAttribute('data-default-mode', defaultMode);
        }
        if (resp && typeof resp.auto_screensaver_seconds === 'number') {
          autoSeconds = resp.auto_screensaver_seconds;
          container.setAttribute('data-auto-screensaver-seconds', String(autoSeconds));
        }
        // Re-arm the timer in case autoSeconds or the active-mode
        // semantics changed.
        resetAutoTimer();
        showToast('Gespeichert', 'success', 1500);
      })
      .catch(function () {
        showToast('Speichern fehlgeschlagen', 'error', 3000);
      });
  }

  if (settingsForm) {
    var settingsInputs = settingsForm.querySelectorAll('input[type="radio"]');
    for (var si = 0; si < settingsInputs.length; si++) {
      settingsInputs[si].addEventListener('change', saveSettings);
    }
    // Defence in depth: if a JS-off-then-on browser still submits
    // the form via Enter on a radio, intercept and route through
    // saveSettings so we never do a full page reload from the
    // inline mode.
    settingsForm.addEventListener('submit', function (e) {
      e.preventDefault();
      saveSettings();
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
        // S14-03-FIX03 Sub-2: opening history marks rows read on
        // the server (handler_mieter_history.go does it
        // asynchronously after the JSON ships), so the badge can
        // drop to 0 right away. SSE will reaffirm with a
        // unread_count frame.
        updateUnreadBadge(0);
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
