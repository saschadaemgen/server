// Central editor state — Svelte 5 runes module.
import catalogJson from './catalog.json';
import graphJson from './graph.json';

// ---- catalog (data-driven palette) ----
// Loaded from bundled mock; in production the server serves catalog.json.
// Replace with a fetch('/a/designer/catalog.json') + this import as fallback.
export let catalog = catalogJson;
export const blocksByType = Object.fromEntries(catalog.blocks.map((b) => [b.type, b]));

export async function loadCatalog() {
  try {
    const res = await fetch('/a/designer/catalog.json', { cache: 'no-store' });
    if (res.ok) {
      const j = await res.json();
      if (j && Array.isArray(j.blocks)) {
        catalog = j;
        for (const k of Object.keys(blocksByType)) delete blocksByType[k];
        for (const b of j.blocks) blocksByType[b.type] = b;
      }
    }
  } catch (_) { /* keep bundled fallback */ }
}

// ---- canonical graph -> xyflow shape ----
function toFlow(g) {
  const nodes = g.nodes.map((n) => ({
    id: n.id,
    type: 'block',
    position: { x: n.ui.x, y: n.ui.y },
    data: { blockType: n.type, params: { ...n.params } },
  }));
  const edges = g.edges.map((e, i) => {
    const [sn, sh] = e.from.split(':');
    const [tn, th] = e.to.split(':');
    return { id: 'e' + i, source: sn, sourceHandle: sh, target: tn, targetHandle: th, type: 'signal' };
  });
  return { nodes, edges };
}
const init = toFlow(graphJson);

// reactive editor state
export const flow = $state({ nodes: init.nodes, edges: init.edges });
export const ui = $state({
  signalFlow: !matchMedia('(prefers-reduced-motion: reduce)').matches,
  running: false,
  activated: true,
  revision: 7,
  dirty: false,
  selectedNode: null,
});

// placeholder issues — the real validator (server) fills these later.
// shape: { severity:'error'|'warning', node_id?, edge_id?, code, message }
export const issues = $state([
  { severity: 'warning', node_id: 'stair', code: 'W_DEFAULT_DURATION',
    message: 'Staircase light uses the default duration (3 s).' },
]);

// ---- canonical export (matches Go ParseGraph/Build) ----
export function toCanonical() {
  return {
    schema: 1,
    nodes: flow.nodes.map((n) => ({
      id: n.id, type: n.data.blockType, params: { ...n.data.params },
      ui: { x: Math.round(n.position.x), y: Math.round(n.position.y) },
    })),
    edges: flow.edges.map((e) => ({ from: `${e.source}:${e.sourceHandle}`, to: `${e.target}:${e.targetHandle}` })),
  };
}

// ---- connection validation (kind match + fan-in forbidden) ----
function typeOf(id) { return flow.nodes.find((n) => n.id === id)?.data.blockType; }
export function isValidConnection(c) {
  if (!c.source || !c.target || c.source === c.target) return false;
  const sB = blocksByType[typeOf(c.source)];
  const tB = blocksByType[typeOf(c.target)];
  const sp = sB?.outputs.find((o) => o.name === c.sourceHandle);
  const tp = tB?.inputs.find((i) => i.name === c.targetHandle);
  if (!sp || !tp) return false;
  if (sp.kind !== tp.kind) return false;                  // data types must match
  // fan-in: an input accepts at most one wire
  if (flow.edges.some((e) => e.target === c.target && e.targetHandle === c.targetHandle)) return false;
  return true;
}

// ---- undo / redo ----
let past = [], future = [], last = snap();
function snap() { return JSON.stringify({ n: flow.nodes, e: flow.edges }); }
export function commit() {
  const s = snap();
  if (s === last) return;
  past.push(last); last = s; future.length = 0; ui.dirty = true;
}
function restore(s) { const o = JSON.parse(s); flow.nodes = o.n; flow.edges = o.e; last = s; }
export function undo() { if (past.length) { future.push(last); restore(past.pop()); } }
export function redo() { if (future.length) { past.push(last); restore(future.pop()); } }
export const history = { get canUndo() { return past.length > 0; }, get canRedo() { return future.length > 0; } };
