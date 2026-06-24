<script>
  // Background on its OWN GPU layer (canvas), decoupled from Svelte re-render:
  // it runs its own rAF loop and never reads component state.
  import { onMount } from 'svelte';

  let canvas;
  onMount(() => {
    const ctx = canvas.getContext('2d');
    const reduce = matchMedia('(prefers-reduced-motion: reduce)').matches;
    let raf, dpr = Math.min(2, devicePixelRatio || 1);
    const blobs = [
      { x: .62, y: .24, r: 460, c: '59,130,246', a: .07, ph: 0 },
      { x: .18, y: .82, r: 380, c: '52,228,234', a: .05, ph: 2.1 },
      { x: .86, y: .66, r: 320, c: '120,90,240', a: .045, ph: 4.2 },
    ];
    function size() {
      dpr = Math.min(2, devicePixelRatio || 1);
      canvas.width = innerWidth * dpr; canvas.height = innerHeight * dpr;
      canvas.style.width = innerWidth + 'px'; canvas.style.height = innerHeight + 'px';
    }
    function draw() {
      const W = canvas.width, H = canvas.height, T = reduce ? 0 : performance.now() / 1000;
      ctx.clearRect(0, 0, W, H);
      ctx.globalCompositeOperation = 'lighter';
      for (const b of blobs) {
        const px = (b.x * innerWidth + Math.sin(T * .06 + b.ph) * 46) * dpr;
        const py = (b.y * innerHeight + Math.cos(T * .05 + b.ph) * 34) * dpr;
        const g = ctx.createRadialGradient(px, py, 0, px, py, b.r * dpr);
        g.addColorStop(0, `rgba(${b.c},${b.a})`); g.addColorStop(1, `rgba(${b.c},0)`);
        ctx.fillStyle = g; ctx.fillRect(0, 0, W, H);
      }
      ctx.globalCompositeOperation = 'source-over';
      // faint static reference dots
      const S = 26 * dpr;
      ctx.fillStyle = '#9fc3d2';
      const cx = W / 2, cy = H / 2, maxD = Math.hypot(cx, cy);
      for (let x = 0; x < W; x += S) for (let y = 0; y < H; y += S) {
        const a = Math.max(0, .26 - Math.hypot(x - cx, y - cy) / maxD * .26);
        if (a < .012) continue;
        ctx.globalAlpha = a; ctx.beginPath(); ctx.arc(x, y, dpr, 0, 6.283); ctx.fill();
      }
      ctx.globalAlpha = 1;
    }
    function loop() { draw(); raf = requestAnimationFrame(loop); }
    size(); addEventListener('resize', size);
    if (reduce) draw(); else loop();
    return () => { cancelAnimationFrame(raf); removeEventListener('resize', size); };
  });
</script>

<canvas bind:this={canvas} class="bg"></canvas>

<style>
  .bg{position:absolute;inset:0;width:100%;height:100%;z-index:0;pointer-events:none;
    background:
      radial-gradient(960px 600px at 60% 24%, rgba(22,46,52,.30) 0%, transparent 62%),
      linear-gradient(180deg,#08090B,#050506);}
</style>
