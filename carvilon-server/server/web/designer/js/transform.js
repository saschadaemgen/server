// Transform: the world pan/zoom (S.scale/S.tx/S.ty live in the store).
// applyTransform pushes the current transform onto #world and repaints
// the grid + minimap; graphBounds is the node bounding box; fit frames
// the whole graph in the *visible* viewport (topbar above, dock below,
// rail/inspector to the sides). Pan+zoom are remembered per graph so a
// reload returns to the exact viewport the user left.

import { S, nodes, world, view } from './store.js';
import { drawBG } from './background.js';
import { renderMinimap, updateMinimap } from './minimap.js';
import { recomputeEndpoints } from './wires.js';

/* ===== per-graph view persistence (survives reload + graph switch) ===== */
// Only user-driven views are stored (S.userAdjusted) and only while a
// graph is open (view.graphId); a short debounce coalesces a pan/zoom
// burst into one write. flushView forces the pending write out before a
// graph switch or page unload so the last gesture is never lost.
let viewTimer=null;
const viewKey=()=>'cv_view_'+view.graphId;
function writeView(){try{localStorage.setItem(viewKey(),JSON.stringify({s:S.scale,x:S.tx,y:S.ty}));}catch(_){/* private mode / quota */}}
export function flushView(){if(viewTimer){clearTimeout(viewTimer);viewTimer=null;}if(view.graphId&&S.userAdjusted)writeView();}
function persistView(){if(!view.graphId||!S.userAdjusted||viewTimer)return;viewTimer=setTimeout(()=>{viewTimer=null;if(view.graphId&&S.userAdjusted)writeView();},180);}
// restoreView applies a stored viewport for the current graph. Returns
// true when one was found+applied (the loader then skips the auto-fit).
// It marks the view user-adjusted so the boot/resize auto-fit leaves it be.
export function restoreView(){
  if(!view.graphId)return false;
  let v=null;try{v=JSON.parse(localStorage.getItem(viewKey())||'null');}catch(_){}
  if(!v||!isFinite(v.s)||!isFinite(v.x)||!isFinite(v.y)||v.s<=0)return false;
  S.scale=v.s;S.tx=v.x;S.ty=v.y;S.userAdjusted=true;
  applyTransform();recomputeEndpoints();renderMinimap();updateMinimap();
  return true;
}

// promoteWorld composites #world for a smooth pan/zoom, then DEMOTES it a
// beat after the gesture settles so the browser re-rasterizes the content
// at the current zoom (crisp text). A permanent will-change would leave the
// layer bitmap-scaled and blurry at scale>1.
let layerTimer=null;
function promoteWorld(){
  if(world.style.willChange!=='transform')world.style.willChange='transform';
  if(layerTimer)clearTimeout(layerTimer);
  layerTimer=setTimeout(()=>{world.style.willChange='auto';layerTimer=null;},220);
}
export function applyTransform(){world.style.transform=`translate(${S.tx}px,${S.ty}px) scale(${S.scale})`;const _zv=document.getElementById('rt-zval');if(_zv)_zv.textContent=Math.round(S.scale*100)+'%';promoteWorld();drawBG();updateMinimap();persistView();}
export function graphBounds(){let a=1e9,b=1e9,c=-1e9,d=-1e9;for(const id in nodes){const el=nodes[id].el,x=el.offsetLeft,y=el.offsetTop,w=el.offsetWidth,h=el.offsetHeight;a=Math.min(a,x);b=Math.min(b,y);c=Math.max(c,x+w);d=Math.max(d,y+h);}return{minX:a,minY:b,maxX:c,maxY:d};}
// visibleCanvas returns the on-screen rectangle actually free for the
// stage: the window minus whatever chrome is really present right now —
// the topbar, the bottom dock (bar + body, collapsed or open), the left
// block rail (which display:none's below 980px), the right tool rail,
// and the inspector ONLY while it is shown (it sits translated off-screen
// otherwise). All measured from live rects, so nothing reserves space it
// is not using — the reason a fixed reservation used to shove the graph
// against the left rail and leave a dead gap on the right.
function visibleCanvas(){
  const vw=innerWidth,vh=innerHeight;let x0=0,y0=0,x1=vw,y1=vh;
  const box=el=>el?el.getBoundingClientRect():null;
  const tb=box(document.querySelector('.topbar'));if(tb&&tb.height>0)y0=Math.max(y0,tb.bottom);
  const dk=box(document.getElementById('dock'));if(dk&&dk.height>0&&dk.bottom>=vh-1)y1=Math.min(y1,dk.top);
  const rl=box(document.querySelector('.rail'));if(rl&&rl.width>0&&rl.left<=1)x0=Math.max(x0,rl.right);
  const rt=box(document.getElementById('rtools'));if(rt&&rt.width>0)x1=Math.min(x1,rt.left);
  const insp=document.getElementById('inspector');
  if(insp&&insp.classList.contains('show')){const r=insp.getBoundingClientRect();if(r.width>0)x1=Math.min(x1,r.left);}
  return{x0,y0,x1,y1};
}
// fit frames the graph centred inside that visible rectangle, so it lands
// in the middle of what the user can actually see — dock open or closed,
// rail present or not, inspector open or not.
export function fit(an){
  const bd=graphBounds();
  if(!isFinite(bd.minX)||bd.maxX<bd.minX)return;   // nothing to frame yet
  const pad=150,{x0,y0,x1,y1}=visibleCanvas();
  const availW=Math.max(120,x1-x0),availH=Math.max(120,y1-y0);
  const gw=(bd.maxX-bd.minX)+pad*2,gh=(bd.maxY-bd.minY)+pad*2;
  S.scale=Math.max(.4,Math.min(1.1,availW/gw,availH/gh));
  const cx=bd.minX+(bd.maxX-bd.minX)/2,cy=bd.minY+(bd.maxY-bd.minY)/2;
  S.tx=x0+availW/2-cx*S.scale;
  S.ty=y0+availH/2-cy*S.scale;
  if(an)world.style.transition='transform .5s cubic-bezier(.2,.7,.2,1)';applyTransform();recomputeEndpoints();renderMinimap();updateMinimap();if(an)setTimeout(()=>world.style.transition='',520);}
