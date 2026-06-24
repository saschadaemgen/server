<script>
  import { getBezierPath } from '@xyflow/svelte';
  import { ui, flow, blocksByType } from './store.svelte.js';
  import { KIND } from './categories.js';

  let { id, sourceX, sourceY, targetX, targetY, sourcePosition, targetPosition } = $props();
  const path = $derived(getBezierPath({ sourceX, sourceY, targetX, targetY, sourcePosition, targetPosition })[0]);

  // FX-08: each wire takes the colour of its source port's kind.
  const color = $derived.by(() => {
    const e = flow.edges.find(x => x.id === id); if (!e) return '#3B82F6';
    const n = flow.nodes.find(x => x.id === e.source);
    const b = n && blocksByType[n.data.blockType];
    const p = b && b.outputs.find(o => o.name === e.sourceHandle);
    return (p && KIND[p.kind]?.color) || '#3B82F6';
  });
</script>

<!-- glow group: drop-shadow on the GROUP (CSS), never feGaussianBlur per edge -->
<g class="wire" style="--wire:{color};--wire-soft:{color}80">
  <path class="base" d={path} />
  {#if ui.signalFlow}
    <path class="signal-flow" d={path} />
  {/if}
</g>

<style>
  .wire{ filter: drop-shadow(0 0 2px var(--wire-soft)) drop-shadow(0 0 8px var(--wire-soft)); }
  .base{ fill:none; stroke:var(--wire); stroke-width:2.4; stroke-linecap:round; }
  .signal-flow{ fill:none; stroke:#EAFEFF; stroke-width:2.1; stroke-linecap:round;
    stroke-dasharray:1 13; opacity:.55; animation:flow 1.6s linear infinite; }
  @keyframes flow{ to{ stroke-dashoffset:-28; } }
  @media (prefers-reduced-motion: reduce){ .signal-flow{ animation:none } }
</style>
