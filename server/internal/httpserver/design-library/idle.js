// Saison 14-01b: mieter idle screensaver runtime.
//
// Three jobs:
//   1. Tick the clock + date every second so the screensaver
//      stays current without server-side renders.
//   2. Poll /einloggen/weather every 15 minutes and patch the
//      DOM. Hide the weather block on backend errors so a
//      degraded screensaver still shows clock + date cleanly.
//   3. Toggle between screensaver and livestream on tap/click.
//      The toggle is page-local; reload returns to the user's
//      persisted default (data-default-mode on #idle-container).
//
// Required DOM (rendered by home.html):
//   <div id="idle-container" data-default-mode="screensaver">
//     <div id="screensaver" class="idle-view ...">
//       <div class="screensaver-clock"></div>
//       <div class="screensaver-date"></div>
//       <div class="screensaver-weather">
//         <i data-icon="cloud"></i>
//         <span class="screensaver-temp"></span>
//         <span class="screensaver-desc"></span>
//       </div>
//     </div>
//     <div id="livestream" class="idle-view ...">
//       <img src="/einloggen/stream.mjpeg" ...>
//     </div>
//   </div>
//
// Locale is hard-coded to de-DE; multi-language lands in a
// later saison.
(function () {
  var clockEl = document.querySelector('.screensaver-clock');
  var dateEl = document.querySelector('.screensaver-date');
  var weatherEl = document.querySelector('.screensaver-weather');
  var tempEl = document.querySelector('.screensaver-temp');
  var descEl = document.querySelector('.screensaver-desc');
  var iconEl = weatherEl ? weatherEl.querySelector('[data-icon]') : null;

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
      // One decimal so we don't render "11.4000001"; round to
      // whole degrees on phone-sized screens since 0.1 deg is
      // below the open-meteo accuracy anyway.
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
    fetch('/einloggen/weather', {
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

  // Tap toggle. Page-local state, no persistence.
  var container = document.getElementById('idle-container');
  var screensaver = document.getElementById('screensaver');
  var livestream = document.getElementById('livestream');
  if (container && screensaver && livestream) {
    container.addEventListener('click', function (e) {
      // Don't swallow clicks on real buttons / links inside the
      // idle views (settings icon, future widgets).
      if (e.target.closest('a, button')) return;
      screensaver.classList.toggle('idle-hidden');
      livestream.classList.toggle('idle-hidden');
    });
  }
})();
