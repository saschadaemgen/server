<script>
  import { catalog } from './store.svelte.js';
  import { CATEGORY } from './categories.js';

  // group catalog blocks by category (data-driven; not hardcoded)
  const groups = $derived.by(() => {
    const m = new Map();
    for (const b of catalog.blocks) {
      if (!m.has(b.category)) m.set(b.category, []);
      m.get(b.category).push(b);
    }
    return [...m.entries()];
  });

  let collapsed = $state({});
  const toggle = (c) => (collapsed[c] = !collapsed[c]);

  // start a palette drag — App listens on the flow pane drop
  function dragStart(e, type) {
    e.dataTransfer.setData('application/carvilon-block', type);
    e.dataTransfer.effectAllowed = 'move';
  }
</script>

<aside class="rail">
  <div class="head"><h2>Blocks</h2><span class="cnt">{catalog.blocks.length}</span></div>
  <div class="search">Search blocks…</div>
  <div class="list">
    {#each groups as [cat, items] (cat)}
      {@const c = CATEGORY[cat] ?? { label: cat, color: '#A78BFA' }}
      <div class="group">
        <button class="glabel" onclick={() => toggle(cat)}>
          <span class="dot" style="--gc:{c.color}"></span>
          <span class="gname">{c.label}</span>
          <span class="gcount">{items.length}</span>
          <span class="chev" class:collapsed={collapsed[cat]}>▾</span>
        </button>
        {#if !collapsed[cat]}
          <div class="items">
            {#each items as b (b.type)}
              <div class="item" draggable="true" ondragstart={(e) => dragStart(e, b.type)} style="--gc:{c.color}">
                <span class="iic"></span><span class="iname">{b.title}</span>
              </div>
            {/each}
          </div>
        {/if}
      </div>
    {/each}
  </div>
</aside>

<style>
  .rail{height:100%;display:flex;flex-direction:column;
    background:linear-gradient(180deg, rgba(8,9,11,.78), rgba(6,7,9,.6));border-right:1px solid var(--border);backdrop-filter:blur(14px)}
  .head{padding:13px 16px 9px;display:flex;align-items:center;justify-content:space-between}
  .head h2{margin:0;font:600 11px/1 var(--mono);letter-spacing:.16em;text-transform:uppercase;color:var(--faint)}
  .cnt{font:500 10px/1 var(--mono);color:var(--faint);padding:3px 6px;border-radius:5px;border:1px solid var(--border)}
  .search{margin:0 12px 10px;padding:8px 11px;border-radius:9px;background:rgba(0,0,0,.3);border:1px solid var(--border);color:var(--faint);font-size:12.5px}
  .list{flex:1;overflow-y:auto;padding:0 8px 14px}
  .group{border-top:1px solid rgba(255,255,255,.06)}
  .group:first-child{border-top:none}
  .glabel{width:100%;display:flex;align-items:center;gap:8px;padding:9px 8px 8px;background:none;border:none;cursor:pointer}
  .dot{width:8px;height:8px;border-radius:3px;background:var(--gc);box-shadow:0 0 8px -1px var(--gc)}
  .gname{flex:1;text-align:left;font:600 10px/1 var(--mono);letter-spacing:.12em;text-transform:uppercase;color:var(--muted)}
  .gcount{font:600 9px/1 var(--mono);color:var(--faint);background:rgba(120,150,180,.08);padding:2px 6px;border-radius:5px}
  .chev{color:var(--faint);font-size:10px;transition:transform .15s}
  .chev.collapsed{transform:rotate(-90deg)}
  .items{display:flex;flex-direction:column}
  .item{display:flex;align-items:center;gap:9px;padding:7px 8px;cursor:grab;border-bottom:1px solid rgba(255,255,255,.05)}
  .item:hover{background:rgba(120,150,180,.07)}
  .iic{width:22px;height:22px;border-radius:6px;flex:none;background:color-mix(in srgb,var(--gc) 11%,transparent);border:1px solid color-mix(in srgb,var(--gc) 22%,transparent)}
  .iname{font-size:11.5px;color:var(--text);font-weight:500;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
</style>
