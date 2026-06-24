<script>
  import { getBezierPath } from '@xyflow/svelte';
  import { ui } from './store.svelte.js';

  let { sourceX, sourceY, targetX, targetY, sourcePosition, targetPosition } = $props();
  const path = $derived(getBezierPath({ sourceX, sourceY, targetX, targetY, sourcePosition, targetPosition })[0]);
</script>

<!-- glow group: drop-shadow on the GROUP (CSS), never feGaussianBlur per edge -->
<g class="wire">
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
</style>
