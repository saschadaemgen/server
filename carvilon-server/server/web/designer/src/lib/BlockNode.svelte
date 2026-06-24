<script>
  import { Handle, Position } from '@xyflow/svelte';
  import { blocksByType } from './store.svelte.js';
  import { CATEGORY, KIND } from './categories.js';

  let { id, data, selected } = $props();
  const block = $derived(blocksByType[data.blockType] ?? { title: data.blockType, inputs: [], outputs: [], params: [], category: 'Logik' });
  const cat = $derived(CATEGORY[block.category] ?? { label: block.category, color: '#A78BFA' });

  // vertical position for a port within its side
  const top = (i, n) => `${((i + 1) / (n + 1)) * 100}%`;
</script>

<div class="node" class:selected style="--cat:{cat.color}">
  <div class="accent"></div>

  <!-- inputs (left) -->
  {#each block.inputs as p, i (p.name)}
    <Handle type="target" position={Position.Left} id={p.name}
            style="top:{top(i, block.inputs.length)};--k:{KIND[p.kind].color}"
            class="port {KIND[p.kind].shape}" title="{p.name} · {p.kind}" />
  {/each}
  <!-- outputs (right) -->
  {#each block.outputs as p, i (p.name)}
    <Handle type="source" position={Position.Right} id={p.name}
            style="top:{top(i, block.outputs.length)};--k:{KIND[p.kind].color}"
            class="port {KIND[p.kind].shape}" title="{p.name} · {p.kind}" />
  {/each}

  <div class="head">
    <div class="ic"></div>
    <div class="titles">
      <div class="cat">{cat.label}</div>
      <div class="title">{block.title}</div>
    </div>
  </div>

  {#if block.params.length}
    <div class="body">
      {#each block.params as p (p.name)}
        <div class="prow"><span class="k">{p.name}</span><span class="v">{data.params?.[p.name] ?? p.default ?? '—'}</span></div>
      {/each}
    </div>
  {/if}
</div>

<style>
  .node{position:relative;width:212px;border-radius:15px;
    background:linear-gradient(180deg, var(--panel), var(--panel-2));
    border:1px solid var(--border);
    box-shadow:0 20px 44px -18px rgba(0,0,0,.85), inset 0 1px 0 rgba(255,255,255,.05);
    backdrop-filter:blur(13px) saturate(1.2)}
  .node::before{content:"";position:absolute;inset:0;border-radius:15px;overflow:hidden;pointer-events:none;
    background:radial-gradient(58% 72% at 14% 4%, color-mix(in srgb,var(--cat) 16%,transparent), transparent 50%)}
  .node.selected{border-color:color-mix(in srgb,var(--cat) 60%,transparent);
    box-shadow:0 24px 56px -18px rgba(0,0,0,.9), 0 0 0 1.5px color-mix(in srgb,var(--cat) 55%,transparent), 0 0 32px -6px var(--cat)}
  .accent{position:absolute;left:13px;right:13px;top:0;height:2px;border-radius:0 0 3px 3px;z-index:1;
    background:linear-gradient(90deg, transparent, var(--cat), transparent);opacity:.6}
  .head{position:relative;z-index:1;display:flex;align-items:center;gap:11px;padding:13px 14px 10px}
  .ic{flex:none;width:34px;height:34px;border-radius:9px;
    background:radial-gradient(120% 120% at 30% 20%, color-mix(in srgb,var(--cat) 26%,transparent), color-mix(in srgb,var(--cat) 6%,transparent));
    border:1px solid color-mix(in srgb,var(--cat) 34%,transparent);box-shadow:0 0 7px -4px var(--cat), inset 0 0 9px -7px var(--cat)}
  .cat{font:600 9.5px/1 var(--mono);letter-spacing:.16em;text-transform:uppercase;color:var(--cat);margin-bottom:4px}
  .title{font:600 13.5px/1.15 var(--font);color:var(--text);white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
  .body{position:relative;z-index:1;padding:11px 14px 13px;border-top:1px solid rgba(255,255,255,.05)}
  .prow{display:flex;align-items:center;justify-content:space-between;gap:10px}
  .prow + .prow{margin-top:7px}
  .k{font:500 11px/1 var(--font);color:var(--muted)}
  .v{font:600 11.5px/1 var(--mono);color:var(--text);white-space:nowrap;
    background:rgba(120,150,180,.08);border:1px solid rgba(120,150,180,.12);padding:4px 7px;border-radius:6px}

  /* ports show their data-type by colour + shape */
  :global(.svelte-flow__handle.port){background:radial-gradient(circle,#07100f 0 32%, var(--k) 37% 64%, transparent 66%);
    border:1px solid color-mix(in srgb,var(--k) 65%,transparent) !important;box-shadow:0 0 8px -1px var(--k)}
  :global(.svelte-flow__handle.port.diamond){border-radius:3px;transform:translateY(-50%) rotate(45deg)}
  :global(.svelte-flow__handle.port.square){border-radius:3px}
</style>
