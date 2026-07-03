// Project tree + persistence: the real folder/graph tree in the left
// rail (backed by the admin-gated designer API, migration 032), graph
// load/switch, the ~1s debounced autosave with its dock save-state,
// the topbar breadcrumb, and the ?g=<id> deep link. The canvas starts
// empty with a subtle loading hint and renders exactly once when the
// selected/deep-linked graph arrives - no flash of stale content.
//
// Rules mirrored from the server (which enforces them): system folders
// (System > Reader) are locked - no rename/delete, no creating inside;
// their rows render with a lock icon and the toolbar disables. Deleting
// needs an in-popover confirmation; rename is inline. Switching graphs
// first stops a running engine graph cleanly, then flushes the pending
// autosave - no silent loss of changes or run state.

import { GRAPH, nodes, wires, wireByEdge, world, graphDirty, markDirty, esc, escAttr } from './store.js';
import { buildNode } from './nodes.js';
import { buildWires, recomputeEndpoints } from './wires.js';
import { renderMinimap } from './minimap.js';
import { fit } from './transform.js';
import { clearSel } from './selection.js';
import { resetWireSelection } from './interactions.js';
import { isRunning, stopRun } from './run.js';

const OPEN_KEY = 'cv_tree_open';
const LAST_KEY = 'cv_last_graph';

let folders = [], graphs = [];        // flat, pre-sorted lists from GET tree
let curGraph = null;                  // {id, folder_id, name, rev} of the open graph
let selKind = null, selId = 0;        // tree selection the toolbar acts on ('f'|'g')
let editing = null;                   // {kind, id} row in inline rename
let confirmDel = null;                // {kind, id, name, err?} pending delete
let notice = null;                    // transient error line in the actions slot
const openSet = new Set();

// ---- autosave state ----
let dirty = false, saving = false, queued = false, saveTimer = null;
// lastSaved is the serialized canvas as the server knows it; '' forces
// the next flush/save to post (used when openGraph adopts an orphan
// canvas into a never-saved graph).
let lastSaved = null;

const railEl = document.querySelector('.rail');
const projPop = document.getElementById('proj-pop');
const projBtn = document.getElementById('proj-btn');
const projCur = document.getElementById('proj-cur');

// clearLoading removes the boot loading hint - called exactly once the
// initial graph has rendered (or turned out not to exist/load).
function clearLoading(){
  const el = document.getElementById('canvas-loading');
  if (el) el.remove();
}

// api wraps the designer endpoints (relative to /a/designer/). A
// redirect means the admin session expired (the middleware 303s to the
// login page, which fetch would happily follow into HTML).
async function api(path, body, raw){
  const opts = { credentials: 'same-origin' };
  if (body !== undefined || raw !== undefined){
    opts.method = 'POST';
    opts.headers = { 'Content-Type': 'application/json' };
    opts.body = raw !== undefined ? raw : JSON.stringify(body);
  }
  const res = await fetch(path, opts);
  if (res.redirected) throw Object.assign(new Error('Sitzung abgelaufen'), { status: 401 });
  let data = {};
  try { data = await res.json(); } catch(_) {}
  if (!res.ok) throw Object.assign(new Error(data.error || ('HTTP ' + res.status)), { status: res.status });
  return data;
}

// ---- model helpers ----
function folderById(id){ return folders.find(f => f.id === id) || null; }
function graphById(id){ return graphs.find(g => g.id === id) || null; }
function childFolders(pid){ return folders.filter(f => f.parent_id === pid); }
function folderGraphs(fid){ return graphs.filter(g => g.folder_id === fid); }
function isSysFolder(fid){ const f = folderById(fid); return !!(f && f.system); }
function selectedFolder(){ return selKind === 'f' ? folderById(selId) : null; }
function selectedItem(){ return selKind === 'f' ? folderById(selId) : selKind === 'g' ? graphById(selId) : null; }
// targetFolder is where "new graph" lands: the selected folder, the
// selected graph's folder, the open graph's folder, or the first
// non-system root folder.
function targetFolder(){
  if (selKind === 'f') return folderById(selId);
  if (selKind === 'g'){ const g = graphById(selId); return g ? folderById(g.folder_id) : null; }
  if (curGraph) return folderById(curGraph.folder_id);
  return childFolders(0).find(f => !f.system) || null;
}
// firstGraphId walks the tree in display order (subfolders before a
// folder's own graphs), skipping system folders.
function firstGraphId(){
  function walk(pid){
    for (const f of childFolders(pid)){
      if (f.system) continue;
      const sub = walk(f.id); if (sub) return sub;
      const gs = folderGraphs(f.id); if (gs.length) return gs[0].id;
    }
    return 0;
  }
  return walk(0);
}
function folderPath(fid){
  const parts = []; let f = folderById(fid), hops = 0;
  while (f && hops++ < 12){ parts.unshift(f.name); f = f.parent_id ? folderById(f.parent_id) : null; }
  return parts;
}

async function refetchTree(){
  const tree = await api('tree');
  folders = tree.folders || [];
  graphs = tree.graphs || [];
}

function saveOpen(){ try { localStorage.setItem(OPEN_KEY, JSON.stringify([...openSet])); } catch(_) {} }
function remember(id){
  try { localStorage.setItem(LAST_KEY, String(id)); } catch(_) {}
  try { history.replaceState(null, '', location.pathname + '?g=' + id); } catch(_) {}
}

// ---- painting ----
function renderGraphRow(g, d){
  const isCur = curGraph && curGraph.id === g.id;
  const isSel = selKind === 'g' && selId === g.id;
  const sys = isSysFolder(g.folder_id);
  const name = (editing && editing.kind === 'g' && editing.id === g.id)
    ? `<input class="pt-edit" value="${escAttr(g.name)}">`
    : `<span>${esc(g.name)}</span>`;
  return `<div class="pt-row pt-proj${isCur ? ' cur' : ''}${isSel ? ' sel' : ''}${sys ? ' pt-sys' : ''}" data-gid="${g.id}" style="--d:${d}"><i data-lucide="file"></i>${name}${isCur ? '<i class="pt-dot" data-lucide="check"></i>' : ''}</div>`;
}
function renderFolderRow(f, d){
  const open = openSet.has(f.id);
  const isSel = selKind === 'f' && selId === f.id;
  const name = (editing && editing.kind === 'f' && editing.id === f.id)
    ? `<input class="pt-edit" value="${escAttr(f.name)}">`
    : `<span>${esc(f.name)}</span>`;
  const rows = open
    ? childFolders(f.id).map(c => renderFolderRow(c, d + 1)).join('') +
      folderGraphs(f.id).map(g => renderGraphRow(g, d + 1)).join('')
    : '';
  return `<div class="pt-folder" data-open="${open}"><div class="pt-row pt-fold${isSel ? ' sel' : ''}${f.system ? ' pt-sys' : ''}" data-fid="${f.id}" style="--d:${d}"><i class="pt-chev" data-lucide="chevron-right"></i><i data-lucide="${f.system ? 'lock' : 'folder'}"></i>${name}</div><div class="pt-children">${rows}</div></div>`;
}
function renderActions(){
  if (notice) return `<div class="pt-confirm"><span class="pt-cq">${esc(notice)}</span></div>`;
  if (confirmDel){
    const q = confirmDel.err ? esc(confirmDel.err) : `„${esc(confirmDel.name)}“ löschen?`;
    return `<div class="pt-confirm"><span class="pt-cq">${q}</span>${confirmDel.err ? '' : '<button class="pt-cyes" data-act="delyes">Löschen</button>'}<button class="pt-cno" data-act="delno">Abbrechen</button></div>`;
  }
  const t = targetFolder(), sf = selectedFolder(), sel = selectedItem();
  const selSys = sel && (selKind === 'f' ? sel.system : isSysFolder(sel.folder_id));
  const dis = {
    newp: !t || t.system,
    newf: !!(sf && sf.system),
    ren: !sel || selSys,
    del: !sel || selSys,
  };
  return `<div class="pt-actions">`
    + `<button data-act="newp" class="${dis.newp ? 'dis' : ''}" title="Neuer Graph"><i data-lucide="file-plus"></i></button>`
    + `<button data-act="newf" class="${dis.newf ? 'dis' : ''}" title="Neuer Ordner"><i data-lucide="folder-plus"></i></button>`
    + `<button data-act="ren" class="${dis.ren ? 'dis' : ''}" title="Umbenennen"><i data-lucide="pencil"></i></button>`
    + `<button data-act="del" class="${dis.del ? 'dis' : ''}" title="Löschen"><i data-lucide="trash-2"></i></button></div>`;
}
function paintTree(){
  if (!projPop) return;
  // An async repaint (notice timeout, slow error) must not throw away
  // what the user has typed into a live inline-rename input.
  const prevEdit = projPop.querySelector('.pt-edit');
  const liveVal = prevEdit ? prevEdit.value : null;
  const rows = childFolders(0).map(f => renderFolderRow(f, 0)).join('');
  projPop.innerHTML = `<div class="pt-tree">${rows || '<div class="pt-empty">Kein Baum geladen</div>'}</div>${renderActions()}`;
  if (window.lucide) lucide.createIcons();
  const inp = projPop.querySelector('.pt-edit');
  if (inp){
    if (liveVal !== null) inp.value = liveVal;
    inp.focus();
    if (liveVal === null) inp.select();
    else inp.setSelectionRange(inp.value.length, inp.value.length);
    inp.onpointerdown = ev => ev.stopPropagation();
    inp.onkeydown = ev => {
      if (ev.key === 'Enter'){ ev.preventDefault(); commitRename(inp.value); }
      else if (ev.key === 'Escape'){ editing = null; paintTree(); }
    };
    inp.onblur = () => { if (editing) commitRename(inp.value); };
  }
}
function paintCrumb(){
  const el = document.getElementById('crumb-text');
  if (el) el.textContent = curGraph ? [...folderPath(curGraph.folder_id), curGraph.name].join(' / ') : '—';
  if (projCur) projCur.textContent = curGraph ? curGraph.name : '—';
}
// setSaveState paints the dock segment: rev N / Speichert… /
// Gespeichert · rev N / Speichern fehlgeschlagen.
function setSaveState(state){
  const el = document.getElementById('st-save');
  if (!el) return;
  if (!curGraph){ el.innerHTML = 'rev <b>–</b>'; return; }
  if (state === 'saving') el.textContent = 'Speichert…';
  else if (state === 'saved') el.innerHTML = 'Gespeichert · rev <b>' + curGraph.rev + '</b>';
  else if (state === 'error') el.textContent = 'Speichern fehlgeschlagen';
  else el.innerHTML = 'rev <b>' + curGraph.rev + '</b>';
}
function paintAll(){ paintTree(); paintCrumb(); setSaveState('idle'); }

function flashErr(err){
  notice = (err && err.message) ? err.message : 'Fehler';
  paintTree();
  setTimeout(() => { notice = null; paintTree(); }, 2500);
}

// ---- canvas load / serialize ----
function clearCanvas(){
  clearSel();
  resetWireSelection();
  for (const id in nodes){ nodes[id].el.remove(); delete nodes[id]; }
  for (const o of wires) o.g.remove();
  wires.length = 0;
  for (const k in wireByEdge) delete wireByEdge[k];
  GRAPH.nodes.length = 0;
  GRAPH.edges.length = 0;
}
function loadIntoCanvas(g){
  clearCanvas();
  for (const n of (g && g.nodes) || []){ GRAPH.nodes.push(n); buildNode(n); }
  for (const e of (g && g.edges) || []) GRAPH.edges.push(e);
  buildWires(); renderMinimap(); recomputeEndpoints(); fit(false);
}
// serializeCanvas is the persisted wire format: the full editor defs
// (positions, colors, props, control values) minus the runtime-only
// on/src flags - those change during a run and must not retrigger
// autosave. def.ui is synced from the DOM first because drag/align
// write only style.left/top.
function serializeCanvas(){
  for (const id in nodes){
    const nd = nodes[id];
    nd.def.ui = { x: nd.el.offsetLeft, y: nd.el.offsetTop };
  }
  const ns = GRAPH.nodes.map(n => { const c = { ...n }; delete c.on; delete c.src; return c; });
  return JSON.stringify({ schema: 1, nodes: ns, edges: GRAPH.edges });
}

// ---- autosave ----
function scheduleSave(){
  if (!curGraph) return;
  dirty = true;
  clearTimeout(saveTimer);
  saveTimer = setTimeout(saveNow, 1000);
}
// saveNow POSTs the canvas to the graph it belonged to when the save
// started; the response is applied only if that graph is still open (a
// slow save must not patch the rev/baseline of a switched-to graph).
// Returns whether the content is settled (saved, unchanged, or the
// graph is gone server-side - nothing left to protect).
async function saveNow(){
  if (!curGraph) return true;
  if (saving){ queued = true; return true; }
  const g = curGraph;
  const body = serializeCanvas();
  if (body === lastSaved){ dirty = false; setSaveState('idle'); return true; }
  saving = true;
  setSaveState('saving');
  let ok = false;
  try {
    const data = await api('graphs/' + g.id + '/save', undefined, '{"graph":' + body + '}');
    ok = true;
    if (curGraph === g){
      lastSaved = body; dirty = false;
      g.rev = data.rev;
      const gm = graphById(g.id); if (gm) gm.rev = data.rev;
      setSaveState('saved');
    }
  } catch (err){
    if (curGraph === g) setSaveState('error'); // stays dirty; the next change retries
    if (err && err.status === 404) ok = true;  // graph deleted server-side
  } finally {
    saving = false;
    if (queued){ queued = false; saveNow(); }
  }
  return ok;
}
// flushSave persists outstanding changes before a switch/teardown -
// including an orphan canvas openGraph adopted (lastSaved '') so
// switching away never silently drops what was visible.
// Returns false when the save failed (the caller blocks the switch).
async function flushSave(){
  clearTimeout(saveTimer);
  if (!curGraph) return true;
  while (saving) await new Promise(r => setTimeout(r, 40));
  let ok = true;
  if (dirty || serializeCanvas() !== lastSaved){
    ok = await saveNow();
    while (saving) await new Promise(r => setTimeout(r, 40));
  }
  return ok;
}
// flushBeacon is the page-unload flush: keepalive fetch, fire and
// forget (rev is re-read on the next boot). lastSaved advances only on
// a confirmed 2xx - the tab-switch path (visibilitychange) stays alive,
// and an optimistic baseline would silently suppress every later save
// of the same content.
function flushBeacon(){
  if (!curGraph) return;
  const g = curGraph;
  const body = serializeCanvas();
  if (body === lastSaved) return;
  try {
    fetch('graphs/' + g.id + '/save', {
      method: 'POST', credentials: 'same-origin', keepalive: true,
      headers: { 'Content-Type': 'application/json' },
      body: '{"graph":' + body + '}',
    }).then(res => {
      if (res.ok && !res.redirected && curGraph === g) lastSaved = body;
    }).catch(() => {});
  } catch(_) {}
}

// ---- graph switching ----
// switchSeq makes the LAST click win when two switches race (the slow
// first GET must not overwrite the state of the later one).
let switchSeq = 0;
async function openGraph(id, { keepPopover = false } = {}){
  const seq = ++switchSeq;
  // Kein stiller Verlust: erst den Run sauber stoppen, dann die
  // offenen Aenderungen sichern; schlaegt das Sichern fehl, wird der
  // Wechsel mit Hinweis geblockt statt still verworfen.
  if (isRunning()) await stopRun();
  if (!await flushSave()){
    flashErr({ message: 'Speichern fehlgeschlagen — Wechsel abgebrochen' });
    return;
  }
  let data;
  try { data = await api('graphs/' + id); }
  catch (err){ flashErr(err); return; }
  if (seq !== switchSeq) return;
  // Orphan canvas (curGraph null after a failed boot fetch): unsaved
  // work on the surface is adopted into a never-saved target, and
  // blocks the switch to a stored one - never silently wiped.
  const orphan = curGraph === null && GRAPH.nodes.length > 0;
  if (orphan && data.rev > 0){
    flashErr({ message: 'Ungespeicherte Fläche — zuerst einen neuen Graphen anlegen' });
    return;
  }
  curGraph = { id: data.id, folder_id: data.folder_id, name: data.name, rev: data.rev };
  selKind = 'g'; selId = id;
  if (orphan){
    lastSaved = '';
    dirty = true;
    clearTimeout(saveTimer);
    saveTimer = setTimeout(saveNow, 1000);
  } else {
    loadIntoCanvas(data.graph);
    lastSaved = serializeCanvas();
    dirty = false; clearTimeout(saveTimer);
  }
  remember(id);
  if (!keepPopover && railEl) railEl.classList.remove('proj-open');
  paintAll();
}

// ---- tree actions ----
async function commitRename(value){
  const ed = editing; editing = null;
  if (!ed){ return; }
  const old = ed.kind === 'f' ? folderById(ed.id) : graphById(ed.id);
  const name = (value || '').trim();
  // Deferred repaint: a blur-triggered no-op commit must not rebuild
  // the popover DOM mid-click, or the click that caused the blur lands
  // on nothing (the async rename path repaints after the await anyway).
  if (!old || !name || name === old.name){ setTimeout(paintTree, 0); return; }
  try {
    await api((ed.kind === 'f' ? 'folders/' : 'graphs/') + ed.id + '/rename', { name });
    await refetchTree();
    if (curGraph){
      const g = graphById(curGraph.id);
      if (g){ curGraph.name = g.name; curGraph.folder_id = g.folder_id; }
    }
  } catch (err){ flashErr(err); return; }
  paintAll();
}

async function doDelete(){
  const cd = confirmDel;
  if (!cd) return;
  try {
    const wasCur = cd.kind === 'g' && curGraph && curGraph.id === cd.id;
    if (wasCur){
      if (isRunning()) await stopRun();
      // A pending autosave of the graph being deleted would only 404.
      clearTimeout(saveTimer); dirty = false;
    }
    await api((cd.kind === 'f' ? 'folders/' : 'graphs/') + cd.id + '/delete', {});
    confirmDel = null;
    if (selKind && selId === cd.id && (selKind === 'f') === (cd.kind === 'f')){ selKind = null; selId = 0; }
    await refetchTree();
    if (wasCur){
      curGraph = null;
      // The deleted graph's content leaves the canvas with it (user
      // intent) - so the follow-up open never sees an orphan surface.
      loadIntoCanvas(null);
      const first = firstGraphId();
      if (first){ await openGraph(first, { keepPopover: true }); return; }
      try { localStorage.removeItem(LAST_KEY); } catch(_) {}
      try { history.replaceState(null, '', location.pathname); } catch(_) {}
    }
    paintAll();
  } catch (err){
    confirmDel.err = err.status === 409 ? 'Ordner ist nicht leer' : (err.message || 'Fehler');
    paintTree();
  }
}

async function handleAction(a){
  if (a === 'delno'){ confirmDel = null; paintTree(); return; }
  if (a === 'delyes'){ await doDelete(); return; }
  confirmDel = null;
  if (a === 'ren'){
    const sel = selectedItem();
    if (!sel) return;
    editing = { kind: selKind, id: selId };
    paintTree();
    return;
  }
  if (a === 'del'){
    const sel = selectedItem();
    if (!sel) return;
    confirmDel = { kind: selKind, id: selId, name: sel.name };
    paintTree();
    return;
  }
  if (a === 'newf'){
    const sf = selectedFolder();
    const parent = (sf && !sf.system) ? sf.id : 0;
    try {
      const data = await api('folders', { parent_id: parent, name: 'Neuer Ordner' });
      await refetchTree();
      if (parent){ openSet.add(parent); saveOpen(); }
      selKind = 'f'; selId = data.folder.id;
      editing = { kind: 'f', id: selId };
      paintTree();
    } catch (err){ flashErr(err); }
    return;
  }
  if (a === 'newp'){
    const t = targetFolder();
    if (!t || t.system) return;
    try {
      const data = await api('graphs', { folder_id: t.id, name: 'Neuer Graph' });
      await refetchTree();
      openSet.add(t.id); saveOpen();
      await openGraph(data.graph.id, { keepPopover: true });
      editing = { kind: 'g', id: data.graph.id };
      paintTree();
    } catch (err){ flashErr(err); }
    return;
  }
}

// ---- init ----
export async function initProject(){
  if (!projPop || !projBtn) return;

  try { JSON.parse(localStorage.getItem(OPEN_KEY) || '[]').forEach(id => openSet.add(id)); } catch(_) {}

  // Autosave hooks: the canvas modules report via markDirty (store.js);
  // inspector edits and the on-card slider are caught here by
  // delegation (title/prop inputs, swatches, custom-dropdown picks).
  graphDirty.fn = scheduleSave;
  const insp = document.getElementById('inspector');
  if (insp){
    insp.addEventListener('input', () => markDirty());
    insp.addEventListener('click', e => { if (e.target.closest('.sw,.cv-dd-opt')) markDirty(); });
  }
  world.addEventListener('input', e => { if (e.target.closest('input[data-act=slider]')) markDirty(); });

  // Unload flush: pagehide covers reload/close, hidden covers the
  // admin iframe being swapped away.
  addEventListener('pagehide', flushBeacon);
  document.addEventListener('visibilitychange', () => { if (document.visibilityState === 'hidden') flushBeacon(); });

  projBtn.onclick = e => { e.stopPropagation(); railEl.classList.toggle('proj-open'); };
  document.addEventListener('pointerdown', e => {
    if (!e.target.closest('.rail-head') && !e.target.closest('#proj-pop')) railEl.classList.remove('proj-open');
  });
  projPop.addEventListener('click', e => {
    if (e.target.closest('.pt-edit')) return;
    const act = e.target.closest('[data-act]');
    if (act){ if (!act.classList.contains('dis')) handleAction(act.dataset.act); return; }
    const fold = e.target.closest('.pt-fold');
    if (fold){
      const id = +fold.dataset.fid;
      selKind = 'f'; selId = id; confirmDel = null;
      if (openSet.has(id)) openSet.delete(id); else openSet.add(id);
      saveOpen(); paintTree();
      return;
    }
    const pr = e.target.closest('.pt-proj');
    if (pr){
      const id = +pr.dataset.gid;
      confirmDel = null;
      if (!curGraph || curGraph.id !== id) openGraph(id);
      else { selKind = 'g'; selId = id; paintTree(); }
    }
  });

  try { await refetchTree(); }
  catch (err){
    clearLoading();
    if (projCur) projCur.textContent = '—';
    const el = document.getElementById('st-save');
    if (el) el.textContent = 'offline';
    return;
  }
  if (!localStorage.getItem(OPEN_KEY)) childFolders(0).forEach(f => openSet.add(f.id));

  // Deep link beats last-used beats first graph in the tree.
  const qg = parseInt(new URLSearchParams(location.search).get('g') || '', 10);
  const deep = qg > 0 && graphById(qg) ? qg : 0;
  let last = 0;
  try { last = parseInt(localStorage.getItem(LAST_KEY) || '', 10) || 0; } catch(_) {}
  const pick = deep || (graphById(last) ? last : firstGraphId());
  if (!pick){ clearLoading(); paintAll(); return; }

  try {
    // The one boot render: the canvas stays empty (loading hint) until
    // the picked graph is here, then it is built exactly once - no
    // demo template, no flash of stale content.
    const data = await api('graphs/' + pick);
    curGraph = { id: data.id, folder_id: data.folder_id, name: data.name, rev: data.rev };
    selKind = 'g'; selId = pick;
    loadIntoCanvas(data.graph);
    lastSaved = serializeCanvas();
    remember(pick);
    let f = folderById(curGraph.folder_id);
    while (f){ openSet.add(f.id); f = f.parent_id ? folderById(f.parent_id) : null; }
    saveOpen();
  } catch (err){ curGraph = null; }
  clearLoading();
  paintAll();
}
