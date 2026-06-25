// GPIO pin-picker data: the detected line list (fetched once from
// /a/designer/gpio/lines) plus the one-use allocation — a physical line
// claimed by one GPIO block in the graph is not offered to any other,
// like a Loxone terminal. The inspector renders the Line dropdown from
// this; the chosen line still writes params.channel = "gpio:chip:offset",
// so the backend binding (T2) is unchanged.

import { GRAPH } from './store.js';

let cache = null, inflight = null;

// loadLines fetches and caches the detected lines (empty on a non-GPIO
// host). Concurrent callers share one request; a failure caches an empty
// list rather than retrying on every inspector open.
export async function loadLines() {
  if (cache) return cache;
  if (inflight) return inflight;
  inflight = fetch('gpio/lines', { credentials: 'same-origin' })
    .then(r => r.ok ? r.json() : { lines: [] })
    .then(d => { cache = Array.isArray(d.lines) ? d.lines : []; return cache; })
    .catch(() => { cache = []; return cache; });
  return inflight;
}

// claimedLines returns the channel addresses already bound by the OTHER
// GPIO blocks in the graph (exceptId is the node being edited, whose own
// line stays selectable). Both input and output blocks draw from the same
// pool, so one physical line is used at most once.
export function claimedLines(exceptId) {
  const used = new Set();
  for (const n of GRAPH.nodes) {
    if (n.id === exceptId) continue;
    if (n.type !== 'source.channel' && n.type !== 'sink.channel') continue;
    const ch = (n.props || []).find(p => p.param === 'channel');
    if (ch && ch.v) used.add(ch.v);
  }
  return used;
}
