// Recorded history for a readout DEVICE block, rendered INTO THE INSPECTOR
// SIDEBAR - the editor's established detail surface for a selected module
// (never a modal, same rule the Shelly settings follow). This is the Logic
// Editor half of Sensor History H2; the devices cockpit is the other half.
//
// Both halves mount the SAME component (/static/sensor-chart.js) against the
// SAME query API, so a chart is never implemented twice. The component is a
// classic script exposing a global - loaded in index.html next to lucide -
// rather than an ES module, because the cockpit's page script is classic too
// and one file has to serve both.
//
// Before this, a readout.device block fell through to the Shelly device panel
// and showed an empty channel list + "Device not linked.", because it has no
// def.shelly. The inspector now routes it here instead.

import { nodes } from './store.js';

// The chart mounted for the currently inspected node. Only one node is ever
// inspected, so one handle is enough - but it MUST be destroyed on every
// re-render and on deselect, or its fetches and its ResizeObserver outlive
// the panel that owned them.
let live = null;

export function destroyReadoutHistory() {
  if (live) {
    try { live.destroy(); } catch (e) { /* already gone */ }
    live = null;
  }
}

export function renderReadoutInspector(container, nodeId) {
  destroyReadoutHistory();
  const nd = nodes[nodeId];
  if (!nd) return;
  const ro = nd.def.readout || {};
  container.textContent = '';

  const sec = document.createElement('div');
  sec.className = 'si-sec';
  const h = document.createElement('div');
  h.className = 'si-h';
  h.textContent = 'Recorded history';
  sec.appendChild(h);

  // ro.id is the PLAIN device id, which is exactly what the history API keys
  // on - the same id the recorder stores under. The prefixed channel ref
  // ("protect:<id>:temperature") is the engine's namespace and must not be
  // used here.
  if (!ro.id) {
    const none = document.createElement('div');
    none.className = 'si-none';
    none.textContent = 'Device not linked.';
    sec.appendChild(none);
    container.appendChild(sec);
    return;
  }

  const hint = document.createElement('div');
  hint.className = 'si-hint';
  hint.textContent = 'Stored history, averaged over the recording interval. The block’s live output stays real-time and is not affected by this.';
  sec.appendChild(hint);

  const host = document.createElement('div');
  sec.appendChild(host);
  container.appendChild(sec);

  if (!window.CarvilonSensorChart) {
    const err = document.createElement('div');
    err.className = 'si-none';
    err.textContent = 'Chart component unavailable.';
    sec.appendChild(err);
    return;
  }
  // title:'' - the si-h above is the section heading; the component adds only
  // its range switcher and the per-metric charts.
  live = window.CarvilonSensorChart.mount(host, { device: ro.id, title: '' });
}
