<script>
  import { SvelteFlow, Controls, MiniMap, Background, addEdge } from '@xyflow/svelte';
  import BlockNode from './lib/BlockNode.svelte';
  import SignalEdge from './lib/SignalEdge.svelte';
  import Palette from './lib/Palette.svelte';
  import BgCanvas from './lib/BgCanvas.svelte';
  import { CATEGORY } from './lib/categories.js';
  import {
    flow, ui, issues, blocksByType, isValidConnection, commit, undo, redo, history, toCanonical, loadCatalog,
  } from './lib/store.svelte.js';

  const nodeTypes = { block: BlockNode };
  const edgeTypes = { signal: SignalEdge };

  let viewport = $state({ x: 0, y: 0, zoom: 1 });
  let pane;            // flow wrapper element (for drop coords)
  let idc = 0;
  let gridOn = $state(true);
  let snapOn = $state(true);
  const GRID = 26;

  loadCatalog();

  function onconnect(c) {
    if (!isValidConnection(c)) return;
    flow.edges = addEdge({ ...c, type: 'signal', id: 'e' + Date.now() }, flow.edges);
    commit();
  }

  // drag-from-palette -> create node
  function onDrop(e) {
    e.preventDefault();
    const type = e.dataTransfer.getData('application/carvilon-block');
    if (!type || !blocksByType[type]) return;
    const r = pane.getBoundingClientRect();
    const x = (e.clientX - r.left - viewport.x) / viewport.zoom - 106;
    const y = (e.clientY - r.top - viewport.y) / viewport.zoom - 30;
    const id = type.replace(/[^a-z0-9]/gi, '').slice(0, 8) + '_' + (++idc);
    const params = {};
    for (const p of blocksByType[type].params) params[p.name] = p.default ?? '';
    flow.nodes = [...flow.nodes, { id, type: 'block', position: { x, y }, data: { blockType: type, params } }];
    commit();
  }

  function onkey(e) {
    if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 'z') {
      e.preventDefault(); e.shiftKey ? redo() : undo();
    }
  }

  // node/edge issue lookup for colouring
  const issueFor = (id, kind) => issues.find((i) => (kind === 'node' ? i.node_id : i.edge_id) === id);

  // FX-02 part 2: implode nodes on delete, cut-flash edges, before they're removed.
  const reduce = matchMedia('(prefers-reduced-motion: reduce)').matches;
  async function beforeDelete({ nodes: dn, edges: de }) {
    if (!reduce) {
      dn.forEach(n => document.querySelector(`.svelte-flow__node[data-id="${n.id}"]`)?.classList.add('imploding'));
      de.forEach(e => document.querySelector(`.svelte-flow__edge[data-id="${e.id}"] .wire`)?.classList.add('cutflash'));
      await new Promise(r => setTimeout(r, 300));
    }
    return { nodes: dn, edges: de };
  }

  // FX-05: tools, align/distribute (on selected nodes), zoom.
  let tool = $state('select');
  const selNodes = () => flow.nodes.filter(n => n.selected);
  const dim = (n) => ({ w: n.measured?.width ?? 212, h: n.measured?.height ?? 92 });
  function moveSel(map) { flow.nodes = flow.nodes.map(n => map.has(n.id) ? { ...n, position: map.get(n.id) } : n); commit(); }
  function align(type) {
    const ns = selNodes(); if (ns.length < 2) return;
    const r = ns.map(n => { const d = dim(n); return { id:n.id, x:n.position.x, y:n.position.y, w:d.w, h:d.h }; });
    const minX=Math.min(...r.map(o=>o.x)), maxR=Math.max(...r.map(o=>o.x+o.w)), cX=(minX+maxR)/2;
    const minY=Math.min(...r.map(o=>o.y)), maxB=Math.max(...r.map(o=>o.y+o.h)), cY=(minY+maxB)/2;
    const m = new Map(r.map(o => [o.id, { x:o.x, y:o.y }]));
    for (const o of r) { const p = m.get(o.id);
      if(type==='left')p.x=minX; else if(type==='right')p.x=maxR-o.w; else if(type==='hcenter')p.x=cX-o.w/2;
      else if(type==='top')p.y=minY; else if(type==='bottom')p.y=maxB-o.h; else if(type==='vcenter')p.y=cY-o.h/2; }
    if (type==='dh'||type==='dv') { const k=type==='dh'?'x':'y', dk=type==='dh'?'w':'h';
      const s=[...r].sort((a,b)=>(a[k]+a[dk]/2)-(b[k]+b[dk]/2));
      const c0=s[0][k]+s[0][dk]/2, c1=s[s.length-1][k]+s[s.length-1][dk]/2, step=(c1-c0)/(s.length-1);
      s.forEach((o,i)=>{ const p=m.get(o.id); if(type==='dh')p.x=c0+step*i-o.w/2; else p.y=c0+step*i-o.h/2; }); }
    moveSel(m);
  }
  function zoomBy(f) { if(!pane)return; const z=Math.min(2.4,Math.max(.3,viewport.zoom*f)), r=pane.getBoundingClientRect(), cx=r.width/2, cy=r.height/2, k=z/viewport.zoom;
    viewport = { x: cx-(cx-viewport.x)*k, y: cy-(cy-viewport.y)*k, zoom: z }; }

  // FX-07: magnetic auto-wiring (kind-matched, fan-in-free); snaps Y-aligned on stop.
  const typeOf = (id) => flow.nodes.find(n => n.id === id)?.data.blockType;
  const hCenter = (el) => { const r = el.getBoundingClientRect(); return { x: r.left + r.width/2, y: r.top + r.height/2 }; };
  let magnet = null;
  function autoWireDrag(node) {
    if (!node) return; magnet = null;
    const mine = [...document.querySelectorAll(`.svelte-flow__handle[data-nodeid="${node.id}"]`)];
    const others = [...document.querySelectorAll('.svelte-flow__handle')].filter(h => h.getAttribute('data-nodeid') !== node.id);
    let best = null, bestD = 48;
    for (const dh of mine) {
      const dOut = dh.classList.contains('source'), dHid = dh.getAttribute('data-handleid'), dc = hCenter(dh);
      for (const oh of others) {
        const oOut = oh.classList.contains('source'); if (oOut === dOut) continue;
        const oNid = oh.getAttribute('data-nodeid'), oHid = oh.getAttribute('data-handleid');
        const out = dOut ? { node: node.id, h: dHid } : { node: oNid, h: oHid };
        const inn = dOut ? { node: oNid, h: oHid } : { node: node.id, h: dHid };
        if (flow.edges.some(e => e.target === inn.node && e.targetHandle === inn.h)) continue;
        const op = blocksByType[typeOf(out.node)]?.outputs.find(o => o.name === out.h);
        const ip = blocksByType[typeOf(inn.node)]?.inputs.find(i => i.name === inn.h);
        if (!op || !ip || op.kind !== ip.kind) continue;
        const oc = hCenter(oh), d = Math.hypot(dc.x - oc.x, dc.y - oc.y);
        if (d < bestD) { bestD = d; best = { out, inn, dy: (dc.y - oc.y) / viewport.zoom }; }
      }
    }
    magnet = best;
  }
  function autoWireStop(node) {
    if (magnet && node) { const { out, inn, dy } = magnet;
      flow.nodes = flow.nodes.map(n => n.id === node.id ? { ...n, position: { x: n.position.x, y: n.position.y - dy } } : n);
      if (!flow.edges.some(e => e.source===out.node && e.sourceHandle===out.h && e.target===inn.node && e.targetHandle===inn.h))
        flow.edges = addEdge({ source: out.node, sourceHandle: out.h, target: inn.node, targetHandle: inn.h, type: 'signal', id: 'e' + Date.now() }, flow.edges);
      magnet = null;
    }
    commit();
  }
</script>

<svelte:window onkeydown={onkey} />

<div class="editor">
  <!-- top bar -->
  <header class="topbar">
    <div class="crumb"><b>Logic Editor</b><span class="sep">/</span><span class="chip">Staircase · GF Hallway</span></div>
    <div class="spacer"></div>

    <button class="tbtn toggle" class:on={ui.signalFlow} onclick={() => (ui.signalFlow = !ui.signalFlow)}>
      Signal flow<span class="sw"><i></i></span>
    </button>
    <button class="tbtn" class:on={snapOn} onclick={() => (snapOn = !snapOn)} title="Snap to grid">⊞ Snap</button>
    <button class="tbtn" class:on={gridOn} onclick={() => (gridOn = !gridOn)} title="Show grid">▦ Grid</button>
    <span class="sep2"></span>
    <button class="tbtn" disabled={!history.canUndo} onclick={undo} title="Undo (Ctrl+Z)">↶</button>
    <button class="tbtn" disabled={!history.canRedo} onclick={redo} title="Redo (Ctrl+Shift+Z)">↷</button>
    <span class="sep2"></span>
    <button class="tbtn" class:on={ui.running} onclick={() => (ui.running = !ui.running)} title="Simulate / monitor">▶ {ui.running ? 'Stop' : 'Run'}</button>
    <button class="tbtn activate" onclick={() => { ui.activated = true; ui.revision++; ui.dirty = false; }}>
      ⚡ Activate
    </button>
    <span class="rev">rev {ui.revision}{ui.dirty ? ' *' : ''}</span>
  </header>

  <!-- left palette -->
  <div class="rail"><Palette /></div>

  <!-- canvas -->
  <div class="stage" bind:this={pane} role="application"
       ondrop={onDrop} ondragover={(e) => { e.preventDefault(); e.dataTransfer.dropEffect = 'move'; }}>
    <BgCanvas />
    <SvelteFlow bind:nodes={flow.nodes} bind:edges={flow.edges} bind:viewport
                {nodeTypes} {edgeTypes} {isValidConnection} {onconnect}
                defaultEdgeOptions={{ type: 'signal' }} snapGrid={[GRID, GRID]} snapToGrid={snapOn}
                selectionOnDrag={tool === 'select'} panOnDrag={tool === 'hand'}
                onnodedrag={(e, n) => autoWireDrag(n)} onnodedragstop={(e, n) => autoWireStop(n)} onbeforedelete={beforeDelete} fitView proOptions={{ hideAttribution: true }}>
      {#if gridOn}<Background gap={GRID} />{/if}
      <Controls showLock={false} />
      <MiniMap pannable zoomable nodeColor={(n) => (CATEGORY[blocksByType[n.data.blockType]?.category]?.color) ?? '#3B82F6'} />
    </SvelteFlow>

    <!-- FX-05: right tool / align / distribute / zoom bar -->
    <div class="rtools">
      <button class="rt" class:on={tool === 'select'} onclick={() => (tool = 'select')} title="Select tool">⌖</button>
      <button class="rt" class:on={tool === 'hand'} onclick={() => (tool = 'hand')} title="Move tool">✋</button>
      <div class="rsep"></div><div class="rlbl">ALIGN</div>
      <button class="rt" onclick={() => align('left')} title="Align left">⇤</button>
      <button class="rt" onclick={() => align('hcenter')} title="Center horizontal">⇔</button>
      <button class="rt" onclick={() => align('right')} title="Align right">⇥</button>
      <button class="rt" onclick={() => align('top')} title="Align top">⤒</button>
      <button class="rt" onclick={() => align('vcenter')} title="Center vertical">↕</button>
      <button class="rt" onclick={() => align('bottom')} title="Align bottom">⤓</button>
      <div class="rsep"></div><div class="rlbl">DIST</div>
      <button class="rt" onclick={() => align('dh')} title="Distribute horizontally">⇿</button>
      <button class="rt" onclick={() => align('dv')} title="Distribute vertically">⇳</button>
      <div class="rsep"></div><div class="rlbl">ZOOM</div>
      <button class="rt" onclick={() => zoomBy(1.2)} title="Zoom in">+</button>
      <button class="rt" onclick={() => zoomBy(1/1.2)} title="Zoom out">−</button>
      <div class="rzv">{Math.round(viewport.zoom * 100)}%</div>
    </div>

    <!-- validation / issues panel -->
    <div class="issues">
      <div class="ih">Issues <span class="ic">{issues.length}</span></div>
      {#if issues.length === 0}
        <div class="empty">No issues — graph looks valid.</div>
      {:else}
        {#each issues as it (it.code + (it.node_id ?? it.edge_id ?? ''))}
          <div class="issue {it.severity}">
            <span class="sev"></span>
            <div><div class="msg">{it.message}</div><div class="meta">{it.code}{it.node_id ? ' · ' + it.node_id : ''}</div></div>
          </div>
        {/each}
      {/if}
    </div>
  </div>
</div>

<style>
  .topbar{display:flex;align-items:center;gap:8px;padding:0 14px;
    background:rgba(11,12,15,.93);border-bottom:1px solid var(--border);backdrop-filter:blur(16px)}
  .crumb{display:flex;align-items:center;gap:9px;font-size:13px;color:var(--muted)}
  .crumb b{color:#fff;font-weight:600}
  .crumb .sep{color:var(--faint)}
  .chip{padding:5px 10px;border-radius:8px;background:rgba(120,150,180,.07);border:1px solid var(--border);color:var(--text);font-weight:500;font-size:12.5px}
  .spacer{flex:1}
  .tbtn{height:34px;display:flex;align-items:center;gap:9px;padding:0 12px;border-radius:9px;
    background:#15171d;border:1px solid #272b35;color:#c7ced9;font:500 12.5px/1 var(--font);cursor:pointer}
  .tbtn:hover{background:#1b1e26;border-color:#323743;color:#fff}
  .tbtn:disabled{opacity:.4;cursor:not-allowed}
  .tbtn.on{color:#fff;background:linear-gradient(180deg,var(--accent),var(--accent-dim));border-color:transparent}
  .tbtn.activate{color:#04140a;background:linear-gradient(180deg,#43E08A,#22C55E);border-color:transparent;font-weight:600}
  .sw{width:30px;height:17px;border-radius:99px;background:#2a2f3a;position:relative;flex:none}
  .sw i{position:absolute;top:2px;left:2px;width:13px;height:13px;border-radius:50%;background:#cfd6e0;transition:.2s}
  .tbtn.on .sw{background:#fff5}.tbtn.on .sw i{left:15px;background:#fff}
  .sep2{width:1px;height:22px;background:var(--border)}
  .rev{font:600 11px/1 var(--mono);color:var(--faint);margin-left:2px}

  .stage{position:relative}
  .issues{position:absolute;left:16px;bottom:16px;z-index:5;width:300px;max-height:42%;overflow:auto;
    padding:10px;border-radius:12px;background:var(--panel);border:1px solid var(--border);backdrop-filter:blur(14px);box-shadow:0 16px 38px -16px rgba(0,0,0,.8)}
  .ih{display:flex;align-items:center;gap:8px;font:600 10px/1 var(--mono);letter-spacing:.14em;text-transform:uppercase;color:var(--faint);margin-bottom:9px}
  .ih .ic{color:var(--text);background:rgba(120,150,180,.1);padding:2px 6px;border-radius:5px}
  .empty{font-size:12px;color:var(--faint)}
  .issue{display:flex;gap:9px;padding:8px;border-radius:8px;background:rgba(120,150,180,.05);margin-bottom:6px}
  .issue .sev{width:8px;height:8px;border-radius:50%;margin-top:3px;flex:none}
  .issue.warning .sev{background:var(--warn);box-shadow:0 0 8px var(--warn)}
  .issue.error .sev{background:var(--err);box-shadow:0 0 8px var(--err)}
  .msg{font-size:12px;color:var(--text)}
  .meta{font:500 10px/1 var(--mono);color:var(--faint);margin-top:3px}
  :global(.svelte-flow__controls button){background:var(--panel-2);border-bottom:1px solid var(--border);fill:var(--muted)}
  :global(.svelte-flow__minimap){background:rgba(10,11,14,.85);border:1px solid var(--border);border-radius:10px;right:64px !important}
  .rtools{position:absolute;top:0;right:0;bottom:0;width:52px;z-index:6;display:flex;flex-direction:column;align-items:center;gap:3px;padding:8px 0;overflow-y:auto;background:linear-gradient(180deg,rgba(8,9,11,.8),rgba(6,7,9,.62));border-left:1px solid var(--border);backdrop-filter:blur(14px)}
  .rtools::-webkit-scrollbar{width:0}
  .rt{width:32px;height:32px;border:1px solid transparent;background:transparent;color:var(--muted);border-radius:8px;cursor:pointer;font-size:14px;display:grid;place-items:center;flex:none}
  .rt:hover{background:rgba(120,150,180,.12);color:var(--text)}
  .rt.on{background:var(--accent);color:#fff}
  .rsep{width:22px;height:1px;background:var(--border);margin:3px 0;flex:none}
  .rlbl{font:600 7px/1 var(--mono);letter-spacing:.08em;color:var(--faint)}
  .rzv{font:600 9px/1 var(--mono);color:var(--muted);margin-top:2px}
  :global(.svelte-flow__node.imploding){pointer-events:none}
  :global(.svelte-flow__node.imploding .node){animation:nodeImplode .3s ease forwards}
  @keyframes nodeImplode{to{transform:scale(.2) rotate(8deg);opacity:0;filter:blur(4px)}}
  :global(.svelte-flow__edge .wire.cutflash){animation:wireCut .3s ease forwards}
  @keyframes wireCut{0%{filter:drop-shadow(0 0 7px #fff) drop-shadow(0 0 16px var(--wire))}100%{opacity:0}}
</style>
