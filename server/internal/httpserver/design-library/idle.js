// Saison 14-01b-FIX01: mieter idle screensaver runtime.
//
// Three jobs, all scoped to the #idle-container element that
// lives INSIDE the design-library .stream slot. Nothing in here
// touches body, html, or any element outside the container.
//
//   1. Tick the clock + date every second so the screensaver
//      stays current without server-side renders.
//   2. Poll /webviewer/weather every 15 minutes and patch the
//      DOM. Hide the weather block on backend errors so the
//      degraded screensaver still shows clock + date cleanly.
//   3. Toggle between screensaver and livestream on tap/click
//      INSIDE the container only. Clicks on buttons or links
//      anywhere else on the page (unlock-door, mic, history,
//      logout, open-settings) are ignored.
//
// Plus a small navigation hook: the topbar's existing
// data-action="open-settings" button (from intercom-idle.html)
// navigates to /webviewer/settings. The button predates the
// idle-mode work; we just wire it.
//
// Required DOM (rendered by intercom-idle.html):
//   <div class="stream" role="region" ...>
//     <div id="idle-container" data-default-mode="screensaver">
//       <div id="screensaver" class="idle-view ...">
//         <div class="screensaver-clock"></div>
//         <div class="screensaver-date"></div>
//         <div class="screensaver-weather">
//           <span class="screensaver-weather-icon" data-icon="cloud"></span>
//           <span class="screensaver-temp"></span>
//           <span class="screensaver-desc"></span>
//         </div>
//       </div>
//       <div id="livestream" class="idle-view ...">
//         <img src="/webviewer/stream.mjpeg" ...>
//       </div>
//     </div>
//     <div class="stream-meta">...</div>
//   </div>
//
// Locale is hard-coded to de-DE; i18n lands in a later saison.
(function () {
  var container = document.getElementById('idle-container');
  if (!container) {
    // home page may have rendered without the idle-container
    // (legacy template, unit test). Nothing to do.
    return;
  }

  var screensaver = container.querySelector('#screensaver');
  var livestream = container.querySelector('#livestream');
  var clockEl = container.querySelector('.screensaver-clock');
  var dateEl = container.querySelector('.screensaver-date');
  var weatherEl = container.querySelector('.screensaver-weather');
  var tempEl = container.querySelector('.screensaver-temp');
  var descEl = container.querySelector('.screensaver-desc');
  var iconEl = container.querySelector('.screensaver-weather-icon');

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

  // Tap toggle. Strictly scoped to the container element so
  // clicks on unlock-door, mic, history, logout, open-settings
  // or anywhere else on the page pass through to their own
  // handlers without ever flipping the idle mode.
  if (screensaver && livestream) {
    container.addEventListener('click', function (e) {
      // Defence in depth: even if a future template puts a
      // button inside the container, do not swallow its click.
      if (e.target.closest('a, button')) return;
      screensaver.classList.toggle('idle-hidden');
      livestream.classList.toggle('idle-hidden');
    });
  }

  // Topbar gear button -> /webviewer/settings. The button lives
  // in intercom-idle.html with data-action="open-settings"; this
  // is the only handler that picks it up.
  document.addEventListener('click', function (e) {
    var btn = e.target.closest('[data-action="open-settings"]');
    if (!btn) return;
    e.preventDefault();
    window.location.href = '/webviewer/settings';
  });
})();
