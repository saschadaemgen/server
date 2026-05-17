// Saison 14-03-FIX03 V4: CARVILON wordmark width-sync runtime.
//
// The headline ("CARVILON") and the subline ("INTERCOM VIEWER")
// must end up at a controlled width ratio (default 0.85) so the
// subline reads as the second line of a single lockup, not as a
// random separate label. Achieving this in pure CSS with one
// font-size pair is too brittle (the ratio drifts with every
// font swap and every browser-default font fallback), so we
// measure both elements after layout and set the subline's
// letter-spacing such that its rendered width matches the
// target.
//
// Loaded as <script src="/static/carvilon-brand.js"> by every
// page that uses the brand: admin shell, login, mieter home,
// standalone mieter settings.
//
// Re-runs on:
//   - DOMContentLoaded (initial paint)
//   - window load (just in case)
//   - document.fonts.ready (the chosen webfont finished loading
//     and the headline width changes)
//   - two short timers (50ms + 200ms) as belt-and-braces against
//     Safari's flaky font-readiness signaling
(function () {
  function adjustOne(brand) {
    var name = brand.querySelector('.name');
    var sub = brand.querySelector('.sub');
    if (!name || !sub) return;

    // Reset before measuring so the previous run does not skew
    // this iteration.
    sub.style.letterSpacing = '0';

    var nameWidth = name.getBoundingClientRect().width;
    var subWidth = sub.getBoundingClientRect().width;
    if (nameWidth <= 0 || subWidth <= 0) return;

    var ratio = parseFloat(sub.dataset.targetRatio || '0.85');
    if (!isFinite(ratio) || ratio <= 0) ratio = 0.85;
    var targetWidth = nameWidth * ratio;

    // letter-spacing is applied between every pair of glyphs.
    // Distribute the missing pixels across (charCount - 1) gaps.
    var charCount = (sub.textContent || '').trim().length - 1;
    if (charCount <= 0) return;
    var extra = (targetWidth - subWidth) / charCount;
    sub.style.letterSpacing = extra + 'px';
  }

  function adjustAll() {
    var nodes = document.querySelectorAll('.carvilon-brand');
    for (var i = 0; i < nodes.length; i++) adjustOne(nodes[i]);
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', adjustAll);
  } else {
    adjustAll();
  }
  if (document.fonts && document.fonts.ready && document.fonts.ready.then) {
    document.fonts.ready.then(adjustAll);
  }
  window.addEventListener('load', adjustAll);
  // Belt-and-braces for browsers whose font-loading promise
  // resolves before the layout has actually re-flowed.
  setTimeout(adjustAll, 50);
  setTimeout(adjustAll, 200);

  // Expose for tests / future callers that mutate the DOM and
  // want to re-trigger the sync.
  window.unifixCarvilon = { adjust: adjustAll };
})();
