// Toolbar "sparkles" button → a full-height settings panel that slides in
// from the right (same motion as the node inspector) for the stage
// background: the grid size, the subtle "Starlink train" animation, and
// the accent-coloured tick flash. Grid size is applied to the live S.grid
// (snap step + dot spacing); the animation/tick values live on FX. All
// persist to localStorage via saveFX() and are read live by background.js
// / store.js' snap().

import { FX, FX_DEFAULTS, GRID_SIZES, S, saveFX } from './store.js';

const btn=document.getElementById('btn-fx');
if(btn){
  // [key, label, min, max, step, format] for the animation slider rows.
  const SLIDERS=[
    ['rate','Häufigkeit',0.5,30,0.5,v=>(v%1?v.toFixed(1):v)+'/min'],
    ['speed','Tempo',2,20,1,v=>v.toFixed(0)],
    ['length','Länge',4,26,1,v=>v.toFixed(0)],
    ['intensity','Intensität',0.1,1.5,0.05,v=>v.toFixed(2)+'×'],
    ['blur','Glow',0,3,0.1,v=>v.toFixed(1)+'×'],
  ];
  // Tick effect: an accent-coloured flash that brightens the odd dot as a
  // train passes. Its sliders dim when the tick (or the whole animation)
  // is off.
  const TICK_SLIDERS=[
    ['tickRate','Häufigkeit',0,1,0.01,v=>Math.round(v*100)+'%'],
    ['tickIntensity','Intensität',1,3,0.1,v=>v.toFixed(1)+'×'],
  ];
  // Ambient twinkle ("Funkeln"): constant gentle sparkle across the grid.
  const TWINKLE_SLIDERS=[['twinkleRate','Stärke',0,3,0.1,v=>v.toFixed(1)+'×']];
  const ALL=SLIDERS.concat(TICK_SLIDERS,TWINKLE_SLIDERS);
  const row=(k,lbl,mn,mx,st,cls)=>`<div class="fx-row ${cls||''}" data-row="${k}"><span class="fx-lbl">${lbl}</span><input type="range" data-fx="${k}" min="${mn}" max="${mx}" step="${st}"><span class="fx-val" data-val="${k}"></span></div>`;
  const pop=document.createElement('div');pop.id='fx-pop';pop.className='fx-pop';
  pop.innerHTML=
    `<div class="fx-phead"><span class="fx-ptitles"><span class="fx-peyebrow">Logik-Editor</span><span class="fx-ptitle">Einstellungen</span></span><button type="button" class="fx-pclose" data-fx-close aria-label="Schließen"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M18 6 6 18M6 6l12 12"/></svg></button></div>`+
    `<div class="fx-pbody">`+
      `<div class="fx-group">Arbeitsfläche</div>`+
      `<div class="fx-head"><i data-lucide="grid-3x3"></i>Raster</div>`+
      `<div class="fx-row"><span class="fx-lbl">Größe</span><input type="range" data-grid min="0" max="${GRID_SIZES.length-1}" step="1"><span class="fx-val" data-val="grid"></span></div>`+
      `<div class="fx-group">Hintergrund</div>`+
      `<div class="fx-head"><i data-lucide="sparkles"></i>Funkeln</div>`+
      `<div class="fx-row"><span class="fx-lbl">Aktiv</span><button type="button" class="fx-switch" data-fx-twinkle role="switch" aria-label="Funkeln aktiv"><span class="fx-knob"></span></button></div>`+
      TWINKLE_SLIDERS.map(([k,lbl,mn,mx,st])=>row(k,lbl,mn,mx,st,'twdim')).join('')+
      `<div class="fx-head" style="margin-top:14px"><i data-lucide="move-horizontal"></i>Ketten</div>`+
      `<div class="fx-row"><span class="fx-lbl">Aktiv</span><button type="button" class="fx-switch" data-fx-toggle role="switch" aria-label="Ketten aktiv"><span class="fx-knob"></span></button></div>`+
      SLIDERS.map(([k,lbl,mn,mx,st])=>row(k,lbl,mn,mx,st,'dim')).join('')+
      `<div class="fx-head" style="margin-top:14px"><i data-lucide="zap"></i>Tick-Effekt</div>`+
      `<div class="fx-row dim"><span class="fx-lbl">Aktiv</span><button type="button" class="fx-switch" data-fx-tick role="switch" aria-label="Tick-Effekt aktiv"><span class="fx-knob"></span></button></div>`+
      TICK_SLIDERS.map(([k,lbl,mn,mx,st])=>row(k,lbl,mn,mx,st,'dim tickdim')).join('')+
      `<div class="fx-foot"><button type="button" class="fx-reset" data-fx-reset>Zurücksetzen</button></div>`+
    `</div>`;
  document.body.appendChild(pop);

  const sw=pop.querySelector('[data-fx-toggle]'),swTick=pop.querySelector('[data-fx-tick]'),swTw=pop.querySelector('[data-fx-twinkle]');
  function syncEnabled(){const on=!!FX.enabled;sw.classList.toggle('on',on);sw.setAttribute('aria-checked',String(on));pop.classList.toggle('off',!on);}
  function syncTick(){const on=!!FX.tickEnabled;swTick.classList.toggle('on',on);swTick.setAttribute('aria-checked',String(on));pop.classList.toggle('tickoff',!on);}
  function syncTwinkle(){const on=!!FX.twinkle;swTw.classList.toggle('on',on);swTw.setAttribute('aria-checked',String(on));pop.classList.toggle('twinkleoff',!on);}
  function syncSlider(k){const inp=pop.querySelector(`[data-fx="${k}"]`),vEl=pop.querySelector(`[data-val="${k}"]`),fmt=ALL.find(s=>s[0]===k)[5];if(inp)inp.value=FX[k];if(vEl)vEl.textContent=fmt(FX[k]);}
  function syncGrid(){const idx=Math.max(0,GRID_SIZES.indexOf(S.grid));const inp=pop.querySelector('[data-grid]'),vEl=pop.querySelector('[data-val="grid"]');if(inp)inp.value=idx;if(vEl)vEl.textContent=GRID_SIZES[idx]+' px';}
  function syncAll(){syncEnabled();syncTick();syncTwinkle();syncGrid();ALL.forEach(([k])=>syncSlider(k));}

  sw.addEventListener('click',()=>{FX.enabled=!FX.enabled;saveFX();syncEnabled();});
  swTick.addEventListener('click',()=>{FX.tickEnabled=!FX.tickEnabled;saveFX();syncTick();});
  swTw.addEventListener('click',()=>{FX.twinkle=!FX.twinkle;saveFX();syncTwinkle();});
  pop.addEventListener('input',e=>{
    const grid=e.target.closest('[data-grid]');
    if(grid){const size=GRID_SIZES[Math.max(0,Math.min(GRID_SIZES.length-1,parseInt(grid.value,10)))];S.grid=size;FX.gridSize=size;saveFX();syncGrid();return;}
    const inp=e.target.closest('[data-fx]');if(!inp)return;const k=inp.dataset.fx;FX[k]=parseFloat(inp.value);saveFX();syncSlider(k);});
  pop.querySelector('[data-fx-reset]').addEventListener('click',()=>{Object.assign(FX,FX_DEFAULTS);S.grid=GRID_SIZES.includes(FX.gridSize)?FX.gridSize:S.grid;saveFX();syncAll();});

  function close(){pop.classList.remove('show');btn.classList.remove('on');}
  function open(){syncAll();pop.classList.add('show');btn.classList.add('on');if(window.lucide)lucide.createIcons();}
  btn.addEventListener('click',e=>{e.stopPropagation();pop.classList.contains('show')?close():open();});
  pop.querySelector('[data-fx-close]').addEventListener('click',close);
  document.addEventListener('keydown',e=>{if(e.key==='Escape')close();});
  // click anywhere outside the panel (and not on the toolbar button) closes it
  document.addEventListener('pointerdown',e=>{if(pop.classList.contains('show')&&!e.target.closest('#fx-pop')&&!e.target.closest('#btn-fx'))close();});
}
