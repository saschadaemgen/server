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
                onnodedragstop={commit} fitView proOptions={{ hideAttribution: true }}>
      {#if gridOn}<Background gap={GRID} />{/if}
      <Controls showLock={false} />
      <MiniMap pannable zoomable nodeColor={(n) => (CATEGORY[blocksByType[n.data.blockType]?.category]?.color) ?? '#3B82F6'} />
    </SvelteFlow>

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
  :global(.svelte-flow__minimap){background:rgba(10,11,14,.85);border:1px solid var(--border);border-radius:10px}
</style>
