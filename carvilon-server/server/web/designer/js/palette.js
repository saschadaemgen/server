// Palette: the left rail — the searchable block library (grouped by the
// five categories, with per-category activate/deactivate + recolour),
// the favourites strip, and the category filter. (The project tree
// dropdown lives in project.js since it became server-backed.)
//
// initPalette() builds the whole rail; main.js calls it once after every
// module has evaluated (so the node-create drag handlers it wires up are
// already defined). NAME_ICON / NAME_CAT are filled in here and read by
// nodes.js when a dragged block is dropped onto the canvas.

import { CAT, PALETTE, nodes, S, dragghost, markDirty } from './store.js';
import { attachDrag, moveGhost, dropNew } from './nodes.js';
import { renderMinimap } from './minimap.js';

export const NAME_ICON={},NAME_CAT={},NAME_TYPE={},NAME_CHANNEL={},NAME_UNIT={};

// Categories beyond the five base ones (input/logic/time/memory/output)
// surface only when the runtime catalog includes them - "gpio" on a GPIO
// host, "system" where telemetry is readable. Their display metadata lives
// here; CAT carries the base five.
const EXTRA_CATS={gpio:{color:'#5BE0C8',label:'GPIO',icon:'cpu'},system:{color:'#F2A65A',label:'System',icon:'activity'},telegram:{color:'#2AABEE',label:'Telegram',icon:'send'}};

export async function initPalette(){
 /* library — sourced from the Go block catalog (the single source of
    truth). The blocks come back in the catalog's order; we group them
    into [title, icon, implemented] tuples per category so the rest of
    the rail (counts, favourites, filter, drag) renders exactly as
    before. */
 const LIBRARY={input:[],logic:[],time:[],memory:[],output:[]};
 try{
   const res=await fetch('catalog.json',{credentials:'same-origin'});
   if(!res.ok)throw new Error('catalog '+res.status);
   const data=await res.json();
   for(const b of (data.blocks||[])){
     if(!CAT[b.category])CAT[b.category]=EXTRA_CATS[b.category]||{color:'#7f8c99',label:b.category.toUpperCase(),icon:'box'};
     NAME_TYPE[b.title]=b.type;
     if(b.channel)NAME_CHANNEL[b.title]=b.channel;
     if(b.unit)NAME_UNIT[b.title]=b.unit;
     (LIBRARY[b.category]||(LIBRARY[b.category]=[])).push([b.title,b.icon,!!b.implemented]);
   }
 }catch(err){console.error('designer: block catalog load failed',err);}
 const libEl=document.getElementById('lib');
 function mkItem(name,icon,cat,c,implemented){const it=document.createElement('div');it.className='lib-item';it.dataset.name=name;it.dataset.cat=cat;it.dataset.impl=implemented?'1':'0';it.style.setProperty('--gc',c.color);
   if(!implemented)it.title=name+' · Katalog-Eintrag — Engine-Node folgt';
   it.innerHTML=`<span class="li-ic" title="Activate / deactivate"><i data-lucide="${icon}"></i></span><span class="li-name">${name}</span>`;return it;}
 for(const [cat,items] of Object.entries(LIBRARY)){
   const c=CAT[cat],g=document.createElement('div');g.className='lib-group collapsed';g.dataset.cat=cat;
   g.dataset.view='active';
   g.innerHTML=`<div class="lib-glabel"><div class="glabel-track"><div class="glabel-colors" role="listbox" aria-label="Kategoriefarbe wählen"></div><div class="glabel-main"><span class="gd" title="Farbe ändern" style="--gc:${c.color}"></span><span class="gname">${c.label}</span><span class="gcount" title="Aktive anzeigen">${items.length}</span><span class="gcount-off zero" title="Ausgeblendete anzeigen">0</span><i class="chev" data-lucide="chevron-down"></i></div></div></div><div class="lib-items"></div>`;
   const iw=g.querySelector('.lib-items');
   for(const [name,icon,implemented] of items){NAME_ICON[name]=icon;NAME_CAT[name]=cat;iw.appendChild(mkItem(name,icon,cat,c,implemented));}
   // Accordion: at most one category open at a time. Clicking a closed
   // group closes the others and opens it; clicking the open one closes it.
   g.querySelector('.lib-glabel').addEventListener('click',()=>{if(g.classList.contains('coloring'))return;const willOpen=g.classList.contains('collapsed');libEl.querySelectorAll('.lib-group').forEach(x=>x.classList.add('collapsed'));if(willOpen)g.classList.remove('collapsed');});
   buildCatColors(g,cat);
   g.querySelector('.lib-glabel .gd').addEventListener('click',e=>{e.stopPropagation();openColoring(g,cat);});
   g.querySelector('.gcount').addEventListener('click',e=>{e.stopPropagation();setGroupView(g,'active');});
   g.querySelector('.gcount-off').addEventListener('click',e=>{e.stopPropagation();setGroupView(g,'off');});
   libEl.appendChild(g);
 }
 const railTotal=Object.values(LIBRARY).reduce((a,x)=>a+x.length,0);const cntEl=document.querySelector('.search-cnt');if(cntEl)cntEl.textContent='('+railTotal+')';
 /* The project tree dropdown (folders + graphs, persistence, autosave)
    lives in project.js since it went from demo data to the real
    server-backed tree; main.js initialises it after the palette. */
 /* Category colour picker, "conveyor belt" style: the header label rides
    off to the right and the full palette (PALETTE + a custom-colour chip)
    rides in from the left; picking a colour drives it back. The two panes
    live on one .glabel-track that translates between -50% (label) and 0
    (palette); the slide is pure CSS, this just toggles the .coloring flag.
    buildCatColors fills a group's palette pane once at build time. */
 function buildCatColors(g,cat){const host=g.querySelector('.glabel-colors');if(!host)return;host.innerHTML='';
   const cur=CAT[cat].color.toLowerCase();
   PALETTE.forEach(col=>{const s=document.createElement('button');s.type='button';s.className='gsw'+(col.toLowerCase()===cur?' sel':'');s.style.setProperty('--swc',col);s.dataset.col=col;s.title=col;host.appendChild(s);});
   const cu=document.createElement('button');cu.type='button';cu.className='gsw gsw-custom';cu.title='Eigene Farbe…';cu.setAttribute('aria-label','Eigene Farbe wählen');
   cu.innerHTML='<i data-lucide="pipette"></i>';
   const inp=document.createElement('input');inp.type='color';inp.className='gsw-input';inp.value=/^#[0-9a-f]{6}$/i.test(CAT[cat].color)?CAT[cat].color:'#2dd4ef';cu.appendChild(inp);
   inp.addEventListener('input',()=>setCategoryColor(cat,inp.value));
   inp.addEventListener('change',()=>{setCategoryColor(cat,inp.value);closeColoring();});
   host.appendChild(cu);
   host.addEventListener('click',e=>{const sw=e.target.closest('.gsw');if(!sw||sw.classList.contains('gsw-custom'))return;const col=sw.dataset.col;if(col){setCategoryColor(cat,col);closeColoring();}});
 }
 let coloringGroup=null;
 function openColoring(g,cat){if(coloringGroup&&coloringGroup!==g)closeColoring();buildCatColors(g,cat);g.classList.add('coloring');coloringGroup=g;if(window.lucide)lucide.createIcons();}
 function closeColoring(){if(coloringGroup){coloringGroup.classList.remove('coloring');coloringGroup=null;}}
 // click outside the open palette (and not on the dot that opened it) drives it back; Esc too.
 document.addEventListener('pointerdown',e=>{if(!coloringGroup)return;if(e.target.closest('.glabel-colors'))return;if(coloringGroup.contains(e.target)&&e.target.closest('.gd'))return;closeColoring();},true);
 document.addEventListener('keydown',e=>{if(e.key==='Escape')closeColoring();});
 function setCategoryColor(cat,col){CAT[cat].color=col;
   document.querySelectorAll(`.lib-group[data-cat="${cat}"] .gd`).forEach(d=>d.style.setProperty('--gc',col));
   document.querySelectorAll(`.lib-item[data-cat="${cat}"]`).forEach(it=>it.style.setProperty('--gc',col));
   let touched=false;
   for(const id in nodes){if(nodes[id].def.cat===cat){nodes[id].def.color=col;nodes[id].el.style.setProperty('--cat',col);touched=true;}}
   if(touched)markDirty(); // def.color is persisted state
   const fp=document.querySelector(`#filter-pop .fp-row[data-cat="${cat}"]`);if(fp)fp.style.setProperty('--c',col);
   const cg=document.querySelector(`.lib-group[data-cat="${cat}"] .glabel-colors`);if(cg)cg.querySelectorAll('.gsw[data-col]').forEach(s=>s.classList.toggle('sel',s.dataset.col.toLowerCase()===col.toLowerCase()));
   if(typeof renderMinimap==='function')renderMinimap();}
 /* favourites: a fixed row of six slots above the library. A fresh
    install starts empty — every free slot shows a plus whose popover
    explains the long-press capture. Picks fill the row from the left,
    cap at six, and persist in localStorage across reloads. */
 const FAV_MAX=6,FAV_KEY='cv_favorites';
 let favorites=[];
 try{const s=JSON.parse(localStorage.getItem(FAV_KEY)||'[]');if(Array.isArray(s))favorites=s.filter(n=>typeof n==='string'&&NAME_CAT[n]).slice(0,FAV_MAX);}catch(_){/* fresh start */}
 function saveFavorites(){try{localStorage.setItem(FAV_KEY,JSON.stringify(favorites));}catch(_){/* private mode */}}
 const favSec=document.createElement('div');favSec.className='fav-sec';favSec.innerHTML='<div class="fav-row" id="fav-row"></div>'
   +'<div class="fav-pop" id="fav-pop" role="tooltip">Noch frei — das Symbol eines Moduls in der Liste <b>lange gedrückt halten</b>, um es hier als Favorit abzulegen.</div>';
 libEl.parentNode.insertBefore(favSec,libEl);const favRow=favSec.querySelector('#fav-row');
 const favPop=favSec.querySelector('#fav-pop');
 document.addEventListener('pointerdown',e=>{if(!e.target.closest('#fav-pop')&&!e.target.closest('.fav-slot'))favPop.classList.remove('show');});
 document.addEventListener('keydown',e=>{if(e.key==='Escape')favPop.classList.remove('show');});
 function renderFavorites(){favRow.innerHTML='';favorites.forEach(name=>{const cat=NAME_CAT[name];if(!cat)return;const it=document.createElement('div');it.className='fav-item';it.dataset.name=name;it.dataset.cat=cat;it.style.setProperty('--gc',CAT[cat].color);it.title=name+' · hold to remove';
   it.innerHTML=`<i data-lucide="${NAME_ICON[name]||CAT[cat].icon}"></i>`;
   favRow.appendChild(it);attachFav(it);});
   for(let i=favorites.length;i<FAV_MAX;i++){const s=document.createElement('button');s.type='button';s.className='fav-slot';s.title='Freier Favoriten-Platz';s.setAttribute('aria-label','Freier Favoriten-Platz — Hinweis anzeigen');s.innerHTML='<i data-lucide="plus"></i>';
     s.onclick=e=>{e.stopPropagation();favPop.classList.toggle('show');};favRow.appendChild(s);}
   if(window.lucide)lucide.createIcons();}
 function addFavorite(name){if(!name||favorites.includes(name)||favorites.length>=FAV_MAX)return false;favorites.unshift(name);saveFavorites();renderFavorites();return true;}
 function removeFavorite(name){const i=favorites.indexOf(name);if(i<0)return;favorites.splice(i,1);saveFavorites();renderFavorites();}
 function attachFav(it){it.addEventListener('pointerdown',ev=>{ev.preventDefault();const name=it.dataset.name,cat=it.dataset.cat,sx=ev.clientX,sy=ev.clientY;let started=false;it.classList.add('lp');
   const t=setTimeout(()=>{it.classList.remove('lp');it.classList.add('fav-removing');setTimeout(()=>removeFavorite(name),360);},2000);
   try{it.setPointerCapture(ev.pointerId);}catch(_){}
   function mv(e2){if(!started&&Math.hypot(e2.clientX-sx,e2.clientY-sy)>6){started=true;clearTimeout(t);it.classList.remove('lp');S.newDrag={name,cat};dragghost.style.setProperty('--gc',CAT[cat].color);dragghost.innerHTML=`<span class="gi"><i data-lucide="${NAME_ICON[name]||CAT[cat].icon}"></i></span>${name}`;if(window.lucide)lucide.createIcons();dragghost.classList.add('show');}if(started)moveGhost(e2);}
   function up(e2){clearTimeout(t);it.classList.remove('lp');it.removeEventListener('pointermove',mv);it.removeEventListener('pointerup',up);if(started)dropNew(e2,it);}
   it.addEventListener('pointermove',mv);it.addEventListener('pointerup',up);});}
 renderFavorites();
 let lpTimer=null,lpIcon=null,lpLong=false;
 libEl.addEventListener('pointerdown',e=>{const ic=e.target.closest('.li-ic');if(!ic)return;e.stopPropagation();const it=ic.closest('.lib-item');lpIcon=ic;lpLong=false;ic.classList.add('lp');
   lpTimer=setTimeout(()=>{lpLong=true;ic.classList.remove('lp');if(addFavorite(it.dataset.name)){ic.classList.add('lp-done');setTimeout(()=>ic.classList.remove('lp-done'),440);}},2000);},true);
 function lpEnd(){if(lpTimer){clearTimeout(lpTimer);lpTimer=null;}if(lpIcon){lpIcon.classList.remove('lp');lpIcon=null;}}
 libEl.addEventListener('pointerup',lpEnd,true);libEl.addEventListener('pointercancel',lpEnd,true);
 libEl.addEventListener('pointermove',e=>{if(lpTimer&&!e.target.closest('.li-ic'))lpEnd();},true);
 libEl.addEventListener('click',e=>{const ic=e.target.closest('.li-ic');if(!ic)return;if(lpLong){lpLong=false;return;}toggleItem(ic.closest('.lib-item'));});
 function setGroupView(g,view){g.dataset.view=view;g.classList.remove('collapsed');}
 function updateGroupCounts(g){const items=[...g.querySelectorAll('.lib-item')];const off=items.filter(x=>x.classList.contains('inactive')).length;
   g.querySelector('.gcount').textContent=items.length-off;
   const offEl=g.querySelector('.gcount-off');offEl.textContent=off;offEl.classList.toggle('zero',off===0);}
 // click a block's icon to (de)activate it; it stays in the list, just gets
 // the .inactive flag, and the two header counts update. The active/hidden
 // views (header badges) decide which set is shown.
 function toggleItem(it){if(!it)return;const g=it.closest('.lib-group');it.classList.toggle('inactive');updateGroupCounts(g);}
 /* category filter (button at end of search) */
 const activeCats=new Set(Object.keys(CAT));
 const filterPop=document.getElementById('filter-pop'),filterBtn=document.getElementById('filter-btn');
 function buildFilter(){
   filterPop.innerHTML='<div class="filter-head"><span>Categories</span><button id="fp-all">All</button></div>';
   for(const [key,c] of Object.entries(CAT)){const row=document.createElement('div');row.className='fp-row on';row.dataset.cat=key;row.style.setProperty('--c',c.color);
     row.innerHTML=`<span class="fp-dot"></span><span class="fp-name">${c.label}</span><span class="fp-check"><i data-lucide="check"></i></span>`;
     row.onclick=()=>{if(activeCats.has(key)){if(activeCats.size>1)activeCats.delete(key);}else activeCats.add(key);syncFilter();};
     filterPop.appendChild(row);}
   if(window.lucide)lucide.createIcons();
   filterPop.querySelector('#fp-all').onclick=()=>{Object.keys(CAT).forEach(k=>activeCats.add(k));syncFilter();};
 }
 function syncFilter(){
   filterPop.querySelectorAll('.fp-row').forEach(r=>r.classList.toggle('on',activeCats.has(r.dataset.cat)));
   document.querySelectorAll('.lib-group').forEach(g=>g.style.display=activeCats.has(g.dataset.cat)?'':'none');
   filterBtn.classList.toggle('active',activeCats.size<Object.keys(CAT).length);
 }
 filterBtn.onclick=e=>{e.stopPropagation();filterPop.classList.toggle('show');};
 document.addEventListener('pointerdown',e=>{if(!e.target.closest('#filter-pop')&&!e.target.closest('#filter-btn'))filterPop.classList.remove('show');});
 buildFilter();
 document.querySelectorAll('.lib-item').forEach(attachDrag);
}
