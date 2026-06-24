<script>
  import { onDestroy } from 'svelte';
  import { ui, flow, blocksByType } from './store.svelte.js';
  import { KIND } from './categories.js';

  let { id, sourceX, sourceY, targetX, targetY } = $props();
  const reduce = matchMedia('(prefers-reduced-motion: reduce)').matches;

  // FX-01 part 2: spring-driven swing. Only runs its rAF while the endpoints move,
  // then settles and stops (no idle work) — so only edges on a moving node animate.
  let swing = $state(0);
  let vel = 0, pmx = null, pmy = null, raf = null;
  function loop() {
    raf = requestAnimationFrame(() => {
      vel += (0 - swing) * 0.12; vel *= 0.86; swing += vel;
      if (Math.abs(vel) < 0.08 && Math.abs(swing) < 0.4) { swing = 0; vel = 0; raf = null; return; }
      loop();
    });
  }
  $effect(() => {
    const mx = (sourceX + targetX) / 2, my = (sourceY + targetY) / 2;
    if (pmx !== null && !reduce) { vel += (my - pmy) * 0.6; if (!raf) loop(); }
    pmx = mx; pmy = my;
  });
  onDestroy(() => raf && cancelAnimationFrame(raf));

  // FX-01 part 1: always softly rounded, hangs (sags) by distance; + swing offset.
  const path = $derived.by(() => {
    const dx = Math.max(24, Math.abs(targetX - sourceX) * 0.5);
    const dist = Math.hypot(targetX - sourceX, targetY - sourceY);
    const sag = Math.min(70, Math.max(0, (dist - 70) * 0.18)) + swing;
    return `M ${sourceX} ${sourceY} C ${sourceX + dx} ${sourceY + sag}, ${targetX - dx} ${targetY + sag}, ${targetX} ${targetY}`;
  });

  // FX-08: wire takes the colour of its source port's kind.
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
