// Entry module for the logic editor. It pulls in every concern for its
// side effects (each module attaches its own listeners on evaluation),
// builds the palette, runs the system clock, and boots the canvas once
// the document and fonts have settled.

import { nodes, GRAPH, S, reduceMotion } from './store.js';
import { sizeCanvas } from './background.js';
import { buildWires, recomputeEndpoints } from './wires.js';
import { renderMinimap, updateMinimap } from './minimap.js';
import { fit } from './transform.js';
import { tick } from './loop.js';
import { initPalette } from './palette.js';
import { initProject } from './project.js';

// Pure side-effect modules (listener wiring, demo nodes, dock feeds).
import './nodes.js';
import './selection.js';
import './inspector.js';
import './sim.js';
import './interactions.js';
import './toolbar.js';
import './settings.js';
import './dock.js';
import './run.js';

/* ===== left-rail block palette, then the project tree ===== */
// initProject waits for the palette: the catalog fetch registers the
// runtime categories (gpio/system/...) a stored graph may reference.
initPalette().then(()=>initProject());

/* ===== top-right system clock ===== */
function tickClock(){const d=new Date(),p=n=>String(n).padStart(2,'0');const tEl=document.getElementById('ck-time'),dEl=document.getElementById('ck-date');if(tEl)tEl.innerHTML=`${p(d.getHours())}:${p(d.getMinutes())}:<span class="sec">${p(d.getSeconds())}</span>`;if(dEl)dEl.textContent=d.toLocaleDateString('de-DE',{weekday:'short',day:'2-digit',month:'short'});}
tickClock();setInterval(tickClock,1000);

/* ===== init ===== */
let started=false;
function boot(){if(started)return;started=true;sizeCanvas();buildWires();renderMinimap();fit(false);
  if(!reduceMotion)GRAPH.nodes.forEach((n,i)=>setTimeout(()=>{const nd=nodes[n.id];if(nd)nd.el.classList.add('in');},100+i*120));
  requestAnimationFrame(tick);[120,400,900].forEach(ms=>setTimeout(()=>{if(!S.userAdjusted)fit(false);else{recomputeEndpoints();renderMinimap();updateMinimap();}},ms));}
if(document.readyState==='complete')setTimeout(boot,30);else addEventListener('load',()=>setTimeout(boot,30));
if(document.fonts&&document.fonts.ready)document.fonts.ready.then(()=>{if(started){recomputeEndpoints();renderMinimap();}});
addEventListener('resize',()=>{sizeCanvas();if(!S.userAdjusted)fit(false);else{recomputeEndpoints();renderMinimap();updateMinimap();}});
/* ResizeObserver catches the iframe's late/final sizing that 'resize' misses, recovering fit+wires+minimap from one settled trigger regardless of rAF throttling */
if(window.ResizeObserver){let _lw=0,_lh=0;const ro=new ResizeObserver(()=>{const w=innerWidth,h=innerHeight;if(w===_lw&&h===_lh)return;_lw=w;_lh=h;sizeCanvas();if(!S.userAdjusted)fit(false);else{recomputeEndpoints();renderMinimap();updateMinimap();}});ro.observe(document.documentElement);ro.observe(document.body);}
