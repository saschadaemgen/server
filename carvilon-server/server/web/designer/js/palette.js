// Palette: the left rail — the searchable block library (grouped by the
// five categories, with per-category activate/deactivate + recolour),
// the favourites strip, the category filter, and the project dropdown.
//
// initPalette() builds the whole rail; main.js calls it once after every
// module has evaluated (so the node-create drag handlers it wires up are
// already defined). NAME_ICON / NAME_CAT are filled in here and read by
// nodes.js when a dragged block is dropped onto the canvas.

import { CAT, PALETTE, nodes, S, dragghost } from './store.js';
import { attachDrag, moveGhost, dropNew } from './nodes.js';
import { renderMinimap } from './minimap.js';

export const NAME_ICON={},NAME_CAT={},NAME_TYPE={},NAME_CHANNEL={},NAME_UNIT={};

// Categories beyond the five base ones (input/logic/time/memory/output)
// surface only when the runtime catalog includes them - "gpio" on a GPIO
// host, "system" where telemetry is readable. Their display metadata lives
// here; CAT carries the base five.
const EXTRA_CATS={gpio:{color:'#5BE0C8',label:'GPIO',icon:'cpu'},system:{color:'#F2A65A',label:'System',icon:'activity'}};

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
   g.innerHTML=`<div class="lib-glabel"><span class="gd" style="--gc:${c.color}"></span><span class="gname">${c.label}</span><span class="gcount" title="Aktive anzeigen">${items.length}</span><span class="gcount-off zero" title="Ausgeblendete anzeigen">0</span><i class="chev" data-lucide="chevron-down"></i></div><div class="lib-items"></div>`;
   const iw=g.querySelector('.lib-items');
   for(const [name,icon,implemented] of items){NAME_ICON[name]=icon;NAME_CAT[name]=cat;iw.appendChild(mkItem(name,icon,cat,c,implemented));}
   // Accordion: at most one category open at a time. Clicking a closed
   // group closes the others and opens it; clicking the open one closes it.
   g.querySelector('.lib-glabel').addEventListener('click',()=>{const willOpen=g.classList.contains('collapsed');libEl.querySelectorAll('.lib-group').forEach(x=>x.classList.add('collapsed'));if(willOpen)g.classList.remove('collapsed');});
   g.querySelector('.lib-glabel .gd').addEventListener('click',e=>{e.stopPropagation();openCatColor(cat,e.currentTarget);});
   g.querySelector('.gcount').addEventListener('click',e=>{e.stopPropagation();setGroupView(g,'active');});
   g.querySelector('.gcount-off').addEventListener('click',e=>{e.stopPropagation();setGroupView(g,'off');});
   libEl.appendChild(g);
 }
 const railTotal=Object.values(LIBRARY).reduce((a,x)=>a+x.length,0);const cntEl=document.querySelector('.search-cnt');if(cntEl)cntEl.textContent='('+railTotal+')';
 /* ===== project management dropdown ===== */
 const PROJ=[{type:'folder',name:'Building',open:true,children:[{type:'folder',name:'Ground floor',open:true,children:[{type:'proj',name:'EG · Flur'},{type:'proj',name:'EG · Living'},{type:'proj',name:'EG · Kitchen'}]},{type:'folder',name:'First floor',open:false,children:[{type:'proj',name:'OG · Hall'},{type:'proj',name:'OG · Bath'}]}]},{type:'folder',name:'Sandbox',open:false,children:[{type:'proj',name:'Test rig'}]}];
 let _pid=0;(function tag(ns){ns.forEach(n=>{n._id='p'+(++_pid);if(n.children)tag(n.children);});})(PROJ);
 let curProj='EG · Flur';
 const railEl=document.querySelector('.rail'),projPop=document.getElementById('proj-pop'),projBtn=document.getElementById('proj-btn'),projCur=document.getElementById('proj-cur');
 function findNode(id,ns,parent){for(const n of ns){if(n._id===id)return{n,parent:ns};if(n.children){const r=findNode(id,n.children,n);if(r)return r;}}return null;}
 function renderTree(ns,d){return ns.map(n=>n.type==='folder'?`<div class="pt-folder" data-open="${n.open}"><div class="pt-row pt-fold" data-id="${n._id}" style="--d:${d}"><i class="pt-chev" data-lucide="chevron-right"></i><i data-lucide="folder"></i><span>${n.name}</span></div><div class="pt-children">${n.open?renderTree(n.children,d+1):''}</div></div>`:`<div class="pt-row pt-proj${n.name===curProj?' cur':''}" data-proj="${n.name}" style="--d:${d}"><i data-lucide="file"></i><span>${n.name}</span>${n.name===curProj?'<i class="pt-dot" data-lucide="check"></i>':''}</div>`).join('');}
 function paintProj(){projPop.innerHTML=`<div class="pt-tree">${renderTree(PROJ,0)}</div><div class="pt-actions"><button data-act="newp" title="New project"><i data-lucide="file-plus"></i></button><button data-act="newf" title="New folder"><i data-lucide="folder-plus"></i></button><button data-act="ren" title="Rename"><i data-lucide="pencil"></i></button><button data-act="del" title="Delete"><i data-lucide="trash-2"></i></button></div>`;if(window.lucide)lucide.createIcons();}
 function curParent(ns){for(const n of ns){if(n.type==='proj'&&n.name===curProj)return ns;if(n.children){const r=curParent(n.children);if(r)return r;}}return null;}
 function setCurrent(name){curProj=name;projCur.textContent=name;railEl.classList.remove('proj-open');paintProj();}
 paintProj();
 projBtn.onclick=e=>{e.stopPropagation();railEl.classList.toggle('proj-open');};
 document.addEventListener('pointerdown',e=>{if(!e.target.closest('.rail-head')&&!e.target.closest('#proj-pop'))railEl.classList.remove('proj-open');});
 projPop.addEventListener('click',e=>{
   const fold=e.target.closest('.pt-fold');if(fold){const r=findNode(fold.dataset.id,PROJ);if(r){r.n.open=!r.n.open;paintProj();}return;}
   const pr=e.target.closest('.pt-proj');if(pr){setCurrent(pr.dataset.proj);return;}
   const act=e.target.closest('[data-act]');if(!act)return;const a=act.dataset.act;
   if(a==='newp'){const nm=prompt('New project name');if(nm){(curParent(PROJ)||PROJ[0].children).push({type:'proj',name:nm,_id:'p'+(++_pid)});setCurrent(nm);}}
   else if(a==='newf'){const nm=prompt('New folder name');if(nm){PROJ.push({type:'folder',name:nm,open:true,children:[],_id:'p'+(++_pid)});paintProj();}}
   else if(a==='ren'){const nm=prompt('Rename project',curProj);if(nm){const p=curParent(PROJ);if(p){const it=p.find(x=>x.name===curProj);if(it)it.name=nm;}setCurrent(nm);}}
   else if(a==='del'){const p=curParent(PROJ);if(p){const i=p.findIndex(x=>x.name===curProj);if(i>=0)p.splice(i,1);}let first=null;(function f(ns){for(const n of ns){if(n.type==='proj'&&!first)first=n.name;if(n.children)f(n.children);}})(PROJ);setCurrent(first||'—');}
 });
 function openCatColor(cat,anchor){let pop=document.getElementById('cat-pop');if(!pop){pop=document.createElement('div');pop.id='cat-pop';pop.className='cat-pop';document.body.appendChild(pop);}pop.innerHTML='';
   PALETTE.forEach(col=>{const s=document.createElement('div');s.className='sw'+(col===CAT[cat].color?' sel':'');s.style.background=col;s.style.setProperty('--swc',col);s.onclick=()=>{setCategoryColor(cat,col);pop.remove();};pop.appendChild(s);});
   const r=anchor.getBoundingClientRect();pop.style.left=r.left+'px';pop.style.top=(r.bottom+6)+'px';pop.classList.add('show');
   setTimeout(()=>document.addEventListener('pointerdown',function h(e){if(!e.target.closest('#cat-pop')){pop.remove();document.removeEventListener('pointerdown',h);}}),0);}
 function setCategoryColor(cat,col){CAT[cat].color=col;
   document.querySelectorAll(`.lib-group[data-cat="${cat}"] .gd`).forEach(d=>d.style.setProperty('--gc',col));
   document.querySelectorAll(`.lib-item[data-cat="${cat}"]`).forEach(it=>it.style.setProperty('--gc',col));
   for(const id in nodes){if(nodes[id].def.cat===cat){nodes[id].def.color=col;nodes[id].el.style.setProperty('--cat',col);}}
   const fp=document.querySelector(`#filter-pop .fp-row[data-cat="${cat}"]`);if(fp)fp.style.setProperty('--c',col);
   if(typeof renderMinimap==='function')renderMinimap();}
 const FAV_DEFAULT=['Push-button','Switch','Motion sensor','AND','OR','Staircase light','Timer','Lamp','Relay','Dimmer'];
 const favorites=FAV_DEFAULT.filter(n=>NAME_CAT[n]);
 const favSec=document.createElement('div');favSec.className='fav-sec';favSec.innerHTML='<div class="fav-row" id="fav-row"></div>';
 libEl.parentNode.insertBefore(favSec,libEl);const favRow=favSec.querySelector('#fav-row');
 function renderFavorites(){favRow.innerHTML='';favorites.forEach(name=>{const cat=NAME_CAT[name];if(!cat)return;const it=document.createElement('div');it.className='fav-item';it.dataset.name=name;it.dataset.cat=cat;it.style.setProperty('--gc',CAT[cat].color);it.title=name+' · hold to remove';
   it.innerHTML=`<i data-lucide="${NAME_ICON[name]||CAT[cat].icon}"></i>`;
   favRow.appendChild(it);attachFav(it);});
   if(window.lucide)lucide.createIcons();favSec.style.display=favorites.length?'':'none';}
 function addFavorite(name){if(!name||favorites.includes(name))return;favorites.unshift(name);if(favorites.length>14)favorites.pop();renderFavorites();}
 function removeFavorite(name){const i=favorites.indexOf(name);if(i>=0)favorites.splice(i,1);renderFavorites();}
 function attachFav(it){it.addEventListener('pointerdown',ev=>{ev.preventDefault();const name=it.dataset.name,cat=it.dataset.cat,sx=ev.clientX,sy=ev.clientY;let started=false;it.classList.add('lp');
   const t=setTimeout(()=>{it.classList.remove('lp');it.classList.add('fav-removing');setTimeout(()=>removeFavorite(name),360);},2000);
   try{it.setPointerCapture(ev.pointerId);}catch(_){}
   function mv(e2){if(!started&&Math.hypot(e2.clientX-sx,e2.clientY-sy)>6){started=true;clearTimeout(t);it.classList.remove('lp');S.newDrag={name,cat};dragghost.style.setProperty('--gc',CAT[cat].color);dragghost.innerHTML=`<span class="gi"><i data-lucide="${NAME_ICON[name]||CAT[cat].icon}"></i></span>${name}`;if(window.lucide)lucide.createIcons();dragghost.classList.add('show');}if(started)moveGhost(e2);}
   function up(e2){clearTimeout(t);it.classList.remove('lp');it.removeEventListener('pointermove',mv);it.removeEventListener('pointerup',up);if(started)dropNew(e2,it);}
   it.addEventListener('pointermove',mv);it.addEventListener('pointerup',up);});}
 renderFavorites();
 let lpTimer=null,lpIcon=null,lpLong=false;
 libEl.addEventListener('pointerdown',e=>{const ic=e.target.closest('.li-ic');if(!ic)return;e.stopPropagation();const it=ic.closest('.lib-item');lpIcon=ic;lpLong=false;ic.classList.add('lp');
   lpTimer=setTimeout(()=>{lpLong=true;ic.classList.remove('lp');ic.classList.add('lp-done');setTimeout(()=>ic.classList.remove('lp-done'),440);addFavorite(it.dataset.name);},2000);},true);
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
