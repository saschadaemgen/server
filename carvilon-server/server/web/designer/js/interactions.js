// Interactions: the pointer-driven canvas — pan/drag/marquee selection,
// wheel + button zoom, tool switching, dragging out new wires (with the
// auto-wire-on-drop and gap-clamp helpers), wire selection + cut, the
// align/distribute toolbar, and the keyboard shortcuts. The transient
// interaction state (mode/pan/drag/marquee, the in-flight wire, the
// selected wire) stays module-local; only the shared transform lives on
// S in the store.

import { S, snap, vp, marquee, world, svg, NS, nodes, wires, selection, GRAPH, markDirty } from './store.js';
import { clearSel, refreshSelVisual, selectOnly, toggleSel, selEls } from './selection.js';
import { applyTransform, fit } from './transform.js';
import { socketCenter, findWireTo, addWire, removeWire, recomputeEndpoints, cutWire } from './wires.js';
import { renderMinimap, updateMinimap } from './minimap.js';
import { openInspector } from './inspector.js';
import { firePulse } from './sim.js';
import { deleteSelected } from './nodes.js';

/* ===== pan / drag / marquee ===== */
let mode=null,pan=null,drag=null,mq=null;
let tool='select';        // 'select' (arrow) or 'hand' (pan)
let spaceHand=false;      // Space held → temporary hand, restored on release
let zCount=10;
function bringFront(el){el.style.zIndex=++zCount;}
// The stage cursor reflects the active tool: arrow for select, an open
// hand for the hand tool (or Space held), a closed hand while panning.
// .panning wins over .tool-hand and both cascade onto the nodes (css).
function applyToolCursor(){vp.classList.toggle('tool-hand',(tool==='hand'||spaceHand)&&mode!=='pan');}
function setTool(t){tool=t;document.querySelectorAll('#rtools [data-tool]').forEach(x=>x.classList.toggle('active',x.dataset.tool===t));applyToolCursor();}
function startPan(e){mode='pan';pan={sx:e.clientX,sy:e.clientY,tx:S.tx,ty:S.ty};S.userAdjusted=true;vp.classList.add('panning');applyToolCursor();try{vp.setPointerCapture(e.pointerId);}catch(_){}}
// Rubber-band box-select on empty canvas (select tool). Shift keeps the
// current selection as a base to add to; otherwise it starts fresh.
function startMarquee(e){mode='marquee';const r=vp.getBoundingClientRect();mq={ox:e.clientX,oy:e.clientY,r,base:e.shiftKey?new Set(selection):null};if(!e.shiftKey)clearSel();marquee.style.display='block';marquee.style.left=(e.clientX-r.left)+'px';marquee.style.top=(e.clientY-r.top)+'px';marquee.style.width='0px';marquee.style.height='0px';}
// Pan intent — middle button (always), or the hand tool / Space held on
// the left — is caught in the CAPTURE phase so it wins over the wire /
// port / control handlers below (which stopPropagation): the stage then
// pans from truly anywhere, nodes and wires included, with no selection.
vp.addEventListener('pointerdown',e=>{
  if(e.button===1||((tool==='hand'||spaceHand)&&e.button===0)){e.preventDefault();e.stopPropagation();startPan(e);}
},true);
vp.addEventListener('pointerdown',e=>{
  if(e.button!==0)return;   // right/other buttons (and pan-intent, handled in capture) don't drive select/wire/drag
  const ctl=e.target.closest('[data-noselectdrag]'),nodeEl=e.target.closest('.node'),port=e.target.closest('.port');
  if(port){startWiring(port,e);if(mode){S.userAdjusted=true;try{vp.setPointerCapture(e.pointerId);}catch(_){}}return;}
  deselectWire();
  if(nodeEl&&ctl){ if(!e.shiftKey&&!selection.has(nodeEl.dataset.id))selectOnly(nodeEl.dataset.id); return; }
  if(nodeEl){
    const id=nodeEl.dataset.id;
    if(e.shiftKey)toggleSel(id);else if(!selection.has(id))selectOnly(id);
    if(selection.has(id)){mode='drag';drag={sx:e.clientX,sy:e.clientY,items:selEls().map(el=>({el,ox:el.offsetLeft,oy:el.offsetTop}))};selEls().forEach(bringFront);nodeEl.classList.add('dragging');S.userAdjusted=true;try{vp.setPointerCapture(e.pointerId);}catch(_){}}
  } else {
    // empty canvas + select tool → box-select
    startMarquee(e);try{vp.setPointerCapture(e.pointerId);}catch(_){}
  }
});
vp.addEventListener('pointermove',e=>{
  if(mode==='pan'){S.tx=pan.tx+(e.clientX-pan.sx);S.ty=pan.ty+(e.clientY-pan.sy);applyTransform();}
  else if(mode==='drag'){const dx=(e.clientX-drag.sx)/S.scale,dy=(e.clientY-drag.sy)/S.scale;
    drag.items.forEach(it=>{it.el.style.left=snap(it.ox+dx)+'px';it.el.style.top=snap(it.oy+dy)+'px';});recomputeEndpoints();renderMinimap();updateMinimap();autoWireUpdate();resolveWireClamp();}
  else if(mode==='wire'){updateWireTemp(e);}
  else if(mode==='marquee'){
    const x=Math.min(e.clientX,mq.ox),y=Math.min(e.clientY,mq.oy),w=Math.abs(e.clientX-mq.ox),h=Math.abs(e.clientY-mq.oy);
    marquee.style.left=(x-mq.r.left)+'px';marquee.style.top=(y-mq.r.top)+'px';marquee.style.width=w+'px';marquee.style.height=h+'px';
    selection.clear();if(mq.base)mq.base.forEach(id=>selection.add(id));
    for(const id in nodes){const nr=nodes[id].el.getBoundingClientRect();if(nr.right>x&&nr.left<x+w&&nr.bottom>y&&nr.top<y+h)selection.add(id);}refreshSelVisual();
  }
});
function endPtr(){if(mode==='drag'){document.querySelectorAll('.node.dragging').forEach(n=>n.classList.remove('dragging'));autoWireFinish();markDirty();}if(mode==='marquee')marquee.style.display='none';mode=null;vp.classList.remove('panning');applyToolCursor();}
vp.addEventListener('pointerup',e=>{if(mode==='wire')finishWiring(e);endPtr();});vp.addEventListener('pointercancel',()=>{if(mode==='wire')cancelWiring();endPtr();});
window.addEventListener('pointerup',()=>{if(wiring)cancelWiring();});
vp.addEventListener('wheel',e=>{e.preventDefault();S.userAdjusted=true;const ns=Math.min(2.4,Math.max(.32,S.scale*Math.exp(-e.deltaY*0.0014))),r=ns/S.scale;S.tx=e.clientX-(e.clientX-S.tx)*r;S.ty=e.clientY-(e.clientY-S.ty)*r;S.scale=ns;applyTransform();},{passive:false});
function zoomBtn(d){S.userAdjusted=true;const ns=Math.min(2.4,Math.max(.32,S.scale*(d>0?1.18:1/1.18))),cx=innerWidth/2,cy=innerHeight/2,r=ns/S.scale;S.tx=cx-(cx-S.tx)*r;S.ty=cy-(cy-S.ty)*r;S.scale=ns;world.style.transition='transform .2s ease';applyTransform();setTimeout(()=>world.style.transition='',220);}
document.getElementById('rz-in').onclick=()=>zoomBtn(1);document.getElementById('rz-out').onclick=()=>zoomBtn(-1);
document.getElementById('rz-fit').onclick=()=>{S.userAdjusted=true;fit(true);};
document.querySelectorAll('#rtools [data-tool]').forEach(b=>b.onclick=()=>setTool(b.dataset.tool));
// Space held = temporary hand tool (Figma/Miro convention); release
// returns to the previous tool. Ignored while typing in a field.
addEventListener('keydown',e=>{if(e.code!=='Space'||e.repeat||spaceHand)return;const tag=(e.target.tagName||'').toLowerCase();if(tag==='input'||tag==='textarea'||tag==='select'||tag==='button'||tag==='a'||e.target.isContentEditable)return;spaceHand=true;e.preventDefault();applyToolCursor();});
addEventListener('keyup',e=>{if(e.code==='Space'&&spaceHand){spaceHand=false;applyToolCursor();}});
// Losing focus (Alt-Tab, a click outside the admin iframe) swallows the
// Space keyup, which would otherwise leave the temporary hand tool stuck
// on. Drop it on blur so the pointer never gets wedged in pan mode.
addEventListener('blur',()=>{if(spaceHand){spaceHand=false;applyToolCursor();}});

/* ===== wiring (connect sockets, with whip on connect) ===== */
let wiring=null;
function startWiring(port,e){if(wiring)cancelWiring();const sel=port.dataset.port,dir=port.closest('.ports-out')?'out':'in';mode='wire';const tp=NS('path');tp.setAttribute('class','wire-temp');svg.appendChild(tp);wiring={sel,dir,tp};updateWireTemp(e);}
function updateWireTemp(e){if(!wiring)return;const a=socketCenter(wiring.sel);if(!a)return;const cx=(e.clientX-S.tx)/S.scale,cy=(e.clientY-S.ty)/S.scale,dx=Math.max(40,Math.abs(cx-a.x)*0.5),from=wiring.dir==='out';
  wiring.tp.setAttribute('d',`M ${a.x} ${a.y} C ${a.x+(from?dx:-dx)} ${a.y}, ${cx+(from?-dx:dx)} ${cy}, ${cx} ${cy}`);
  clearTargetHi();const el=document.elementFromPoint(e.clientX,e.clientY),tp=el&&el.closest('.port');
  if(tp){const td=tp.closest('.ports-out')?'out':'in';if(td!==wiring.dir&&tp.dataset.port.split(':')[0]!==wiring.sel.split(':')[0])tp.classList.add('wire-ok');}}
function clearTargetHi(){document.querySelectorAll('.port.wire-ok').forEach(p=>p.classList.remove('wire-ok'));}
function cancelWiring(){if(wiring&&wiring.tp)wiring.tp.remove();wiring=null;clearTargetHi();}
function finishWiring(e){
  try{const el=document.elementFromPoint(e.clientX,e.clientY),tp=el&&el.closest('.port');
    if(tp&&wiring){const tsel=tp.dataset.port,td=tp.closest('.ports-out')?'out':'in';let outSel,inSel;
      if(wiring.dir==='out'&&td==='in'){outSel=wiring.sel;inSel=tsel;}else if(wiring.dir==='in'&&td==='out'){outSel=tsel;inSel=wiring.sel;}
      if(outSel&&inSel&&outSel.split(':')[0]!==inSel.split(':')[0])createEdge(outSel,inSel);}
  }catch(err){console.warn('wire connect failed',err);}finally{cancelWiring();}}
function createEdge(outSel,inSel){if(GRAPH.edges.some(x=>x.from===outSel&&x.to===inSel))return;const ex=findWireTo(inSel);if(ex)removeWire(ex);
  const edge={from:outSel,to:inSel};GRAPH.edges.push(edge);const o=addWire(edge);if(o){o.vel={x:(Math.random()*2-1)*14,y:-34};firePulse(outSel+'>'+inSel);}renderMinimap();markDirty();if(selection.size===1)openInspector([...selection][0]);}
function freePortsForAuto(id){const n=nodes[id].def,r=[];n.ports.in.forEach(p=>{if(!findWireTo(id+':'+p.id))r.push({sel:id+':'+p.id,dir:'in'});});n.ports.out.forEach(p=>r.push({sel:id+':'+p.id,dir:'out'}));return r;}
function autoWireUpdate(){clearTargetHi();if(!drag||drag.items.length!==1)return;
  const id=drag.items[0].el.dataset.id;if(!nodes[id])return;let best=null,bestD=52;
  for(const fp of freePortsForAuto(id)){const a=socketCenter(fp.sel);if(!a)continue;
    for(const oid in nodes){if(oid===id)continue;const on=nodes[oid].def;
      const cands=fp.dir==='out'?on.ports.in.map(p=>({sel:oid+':'+p.id,dir:'in'})):on.ports.out.map(p=>({sel:oid+':'+p.id,dir:'out'}));
      for(const tp of cands){if(tp.dir==='in'&&findWireTo(tp.sel))continue;const b=socketCenter(tp.sel);if(!b)continue;const dd=Math.hypot(a.x-b.x,a.y-b.y);
        if(dd<bestD){bestD=dd;best={out:fp.dir==='out'?fp.sel:tp.sel,in:fp.dir==='out'?tp.sel:fp.sel,dragPort:fp.sel,targetPort:tp.sel};}}}}
  if(best){const a=socketCenter(best.dragPort),b=socketCenter(best.targetPort);
    if(a&&b){const el=nodes[id].el;el.style.top=(el.offsetTop-(a.y-b.y))+'px';recomputeEndpoints();}
    [best.dragPort,best.targetPort].forEach(sel=>{const pe=world.querySelector(`[data-port="${sel}"]`);if(pe)pe.classList.add('wire-ok');});
    if(!GRAPH.edges.some(x=>x.from===best.out&&x.to===best.in))createEdge(best.out,best.in);}}
function autoWireFinish(){clearTargetHi();}
function resolveWireClamp(){if(!drag||drag.items.length!==1)return;const id=drag.items[0].el.dataset.id;if(!nodes[id])return;const el=nodes[id].el,minGap=46;let changed=false;
  for(const o of wires){let mySel,otherSel,iAmOut;
    if(o.from.split(':')[0]===id){mySel=o.from;otherSel=o.to;iAmOut=true;}
    else if(o.to.split(':')[0]===id){mySel=o.to;otherSel=o.from;iAmOut=false;}else continue;
    const a=socketCenter(mySel),b=socketCenter(otherSel);if(!a||!b)continue;
    const dy=a.y-b.y;if(Math.abs(dy)>44)continue;
    const outX=iAmOut?a.x:b.x,inX=iAmOut?b.x:a.x,gap=inX-outX;
    if(gap<minGap){const need=minGap-gap;el.style.left=(el.offsetLeft+(iAmOut?-need:need))+'px';el.style.top=(el.offsetTop-dy)+'px';changed=true;}
    else if(gap<minGap+44){el.style.top=(el.offsetTop-dy)+'px';changed=true;}}
  if(changed){recomputeEndpoints();renderMinimap();updateMinimap();}}
let selectedWire=null;const wireDel=document.getElementById('wire-del');
export function selectWire(o){clearSel();deselectWire();selectedWire=o;o.g.classList.add('wsel');updateWireDel();}
// resetWireSelection drops the floating scissors when the canvas is
// torn down (graph switch) - a stale selection could otherwise cut an
// edge of the newly loaded graph.
export function resetWireSelection(){deselectWire();}
function deselectWire(){if(selectedWire){selectedWire.g.classList.remove('wsel');selectedWire=null;}if(wireDel)wireDel.classList.remove('show');}
function updateWireDel(){if(!selectedWire||!wireDel)return;const sp=selectedWire.sp;wireDel.style.left=(sp.x*S.scale+S.tx)+'px';wireDel.style.top=(sp.y*S.scale+S.ty)+'px';wireDel.classList.add('show');}
if(wireDel)wireDel.onclick=()=>{if(selectedWire){const w=selectedWire;deselectWire();cutWire(w);renderMinimap();markDirty();}};

/* ===== align / distribute ===== */
function alignSel(type){
  const els=selEls();if(els.length<2)return;
  const r=els.map(el=>({el,x:el.offsetLeft,y:el.offsetTop,w:el.offsetWidth,h:el.offsetHeight}));
  const minX=Math.min(...r.map(o=>o.x)),maxR=Math.max(...r.map(o=>o.x+o.w)),cX=(minX+maxR)/2;
  const minY=Math.min(...r.map(o=>o.y)),maxB=Math.max(...r.map(o=>o.y+o.h)),cY=(minY+maxB)/2;
  const tg=new Map();
  if(type==='left')r.forEach(o=>tg.set(o.el,{x:minX,y:o.y}));
  else if(type==='right')r.forEach(o=>tg.set(o.el,{x:maxR-o.w,y:o.y}));
  else if(type==='hcenter')r.forEach(o=>tg.set(o.el,{x:cX-o.w/2,y:o.y}));
  else if(type==='top')r.forEach(o=>tg.set(o.el,{x:o.x,y:minY}));
  else if(type==='bottom')r.forEach(o=>tg.set(o.el,{x:o.x,y:maxB-o.h}));
  else if(type==='vcenter')r.forEach(o=>tg.set(o.el,{x:o.x,y:cY-o.h/2}));
  else if(type==='dh'){const s=[...r].sort((a,b)=>(a.x+a.w/2)-(b.x+b.w/2));const c0=s[0].x+s[0].w/2,c1=s[s.length-1].x+s[s.length-1].w/2,step=(c1-c0)/(s.length-1);s.forEach((o,i)=>tg.set(o.el,{x:c0+step*i-o.w/2,y:o.y}));}
  else if(type==='dv'){const s=[...r].sort((a,b)=>(a.y+a.h/2)-(b.y+b.h/2));const c0=s[0].y+s[0].h/2,c1=s[s.length-1].y+s[s.length-1].h/2,step=(c1-c0)/(s.length-1);s.forEach((o,i)=>tg.set(o.el,{x:o.x,y:c0+step*i-o.h/2}));}
  animateMove(tg);markDirty();
}
function animateMove(tg){
  tg.forEach((p,el)=>{el.style.transition='left .42s cubic-bezier(.2,.7,.2,1),top .42s cubic-bezier(.2,.7,.2,1)';el.style.left=p.x+'px';el.style.top=p.y+'px';});
  const t0=performance.now();(function f(){recomputeEndpoints();renderMinimap();updateMinimap();if(performance.now()-t0<460)requestAnimationFrame(f);else tg.forEach((p,el)=>el.style.transition='');})();
}
document.querySelectorAll('#rtools [data-al]').forEach(b=>b.onclick=()=>alignSel(b.dataset.al));

/* ===== wire-delete cursor follow + keyboard ===== */
function tickWireDel(){if(selectedWire)updateWireDel();requestAnimationFrame(tickWireDel);}requestAnimationFrame(tickWireDel);
addEventListener('keydown',e=>{const tag=(e.target.tagName||'').toLowerCase();if(tag==='input'||tag==='textarea')return;if(e.key==='Escape'){clearSel();deselectWire();}else if(e.key==='Delete'||e.key==='Backspace'){e.preventDefault();if(selectedWire){const w=selectedWire;deselectWire();cutWire(w);renderMinimap();markDirty();}else deleteSelected();}else if(e.key==='v'||e.key==='V')setTool('select');else if(e.key==='h'||e.key==='H')setTool('hand');});
