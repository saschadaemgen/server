// Wires: per-edge patch-cable rendering and physics. Each edge is an
// SVG group (hit path + base + animated flow + travelling sparks) with
// a little spring-and-whip belly that lags endpoint motion. This module
// owns the geometry (socketCenter/pathOf), the per-frame step, edge
// creation/teardown, and the cut animation.

import { world, svg, wires, wireByEdge, GRAPH, reduceMotion, NS, hexRgb } from './store.js';
import { selectWire } from './interactions.js';
import { renderMinimap } from './minimap.js';

export function removeWire(o){o.g.remove();const i=wires.indexOf(o);if(i>=0)wires.splice(i,1);delete wireByEdge[o.from+'>'+o.to];const gi=GRAPH.edges.findIndex(x=>x.from===o.from&&x.to===o.to);if(gi>=0)GRAPH.edges.splice(gi,1);}
export function fxFlare(wx,wy){const f=document.createElement('div');f.className='fx-flare';f.style.left=wx+'px';f.style.top=wy+'px';world.appendChild(f);setTimeout(()=>f.remove(),440);}
export function fxBolt(wx,wy){const f=document.createElement('div');f.className='fx-bolt';f.style.left=wx+'px';f.style.top=wy+'px';world.appendChild(f);setTimeout(()=>f.remove(),320);}
export function cutWire(o){const key=o.from+'>'+o.to;
  // Stale handle (the graph was switched since this wire was selected):
  // never touch the live graph - another graph can carry the same edge
  // key because node ids are deterministic per page load.
  if(wireByEdge[key]!==o){o.g.remove();return;}
  const i=wires.indexOf(o);if(i>=0)wires.splice(i,1);delete wireByEdge[key];const gi=GRAPH.edges.findIndex(x=>x.from===o.from&&x.to===o.to);if(gi>=0)GRAPH.edges.splice(gi,1);
  if(reduceMotion){o.g.remove();return;}
  fxBolt(o.sp.x,o.sp.y);o.g.classList.add('cut');o.vel.x=(Math.random()*2-1)*30;o.vel.y=-32;
  const t0=performance.now(),dur=540;
  (function step(now){const k=Math.min(1,(now-t0)/dur);const mx=(o.e.ax+o.e.bx)/2,my=(o.e.ay+o.e.by)/2,sag=restSag(o);
    o.vel.x+=(mx-o.sp.x)*0.05;o.vel.y+=(my+sag-o.sp.y)*0.05;o.vel.x*=0.9;o.vel.y*=0.9;o.sp.x+=o.vel.x;o.sp.y+=o.vel.y;o.off.x=o.sp.x-mx;o.off.y=o.sp.y-my;
    const d=pathOf(o);o.base.setAttribute('d',d);o.flow.setAttribute('d',d);if(o.hit)o.hit.setAttribute('d',d);
    o.g.style.opacity=String(1-Math.max(0,(k-0.45)/0.55));
    if(k<1)requestAnimationFrame(step);else o.g.remove();})(t0);}

// socketCenter returns a port's centre in #world layout coordinates,
// derived purely from offsetLeft/offsetTop up the offsetParent chain to
// #world. That makes it transform-independent: it is identical at every
// zoom level and mid-transition (unlike getBoundingClientRect()/scale,
// which reads the momentarily-mismatched screen rect while the CSS zoom
// tween is still running and used to snap the wires off their ports).
export function socketCenter(sel){const el=world.querySelector(`[data-port="${sel}"] .socket`);if(!el)return null;
  let x=el.offsetWidth/2,y=el.offsetHeight/2,n=el;
  while(n&&n!==world){x+=n.offsetLeft;y+=n.offsetTop;n=n.offsetParent;}
  return{x,y};}
export function restSag(o){const dx=o.e.bx-o.e.ax,dy=o.e.by-o.e.ay;return Math.min(88,Math.max(0,(Math.hypot(dx,dy)-70)*0.2));}
export function pathOf(o){const{ax,ay,bx,by}=o.e,dx=Math.max(24,Math.abs(bx-ax)*0.5),ox=o.off.x,oy=o.off.y;return`M ${ax} ${ay} C ${ax+dx+ox*1.0} ${ay+oy}, ${bx-dx+ox*1.0} ${by+oy}, ${bx} ${by}`;}
export function stepPhysics(o){const mx=(o.e.ax+o.e.bx)/2,my=(o.e.ay+o.e.by)/2,sag=restSag(o);
  if(reduceMotion){o.off.x=0;o.off.y=sag;o.pm={x:mx,y:my};return;}
  if(!o.pm)o.pm={x:mx,y:my};
  const dmx=mx-o.pm.x,dmy=my-o.pm.y;o.pm.x=mx;o.pm.y=my;
  /* whip: belly lags opposite to endpoint motion, then springs back loosely */
  o.vel.x-=dmx*0.42;o.vel.y-=dmy*0.42;
  o.vel.x+=(mx-o.sp.x)*0.050;o.vel.y+=(my+sag-o.sp.y)*0.050;
  o.vel.x*=0.915;o.vel.y*=0.915;
  const vmax=62;o.vel.x=Math.max(-vmax,Math.min(vmax,o.vel.x));o.vel.y=Math.max(-vmax,Math.min(vmax,o.vel.y));
  o.sp.x+=o.vel.x;o.sp.y+=o.vel.y;
  o.off.x=Math.max(-180,Math.min(180,o.sp.x-mx));o.off.y=Math.max(-60,Math.min(230,o.sp.y-my));}
export function renderWires(){for(const o of wires){const d=pathOf(o);o.base.setAttribute('d',d);o.flow.setAttribute('d',d);if(o.hit)o.hit.setAttribute('d',d);}}
export function recomputeEndpoints(){for(const o of wires){const a=socketCenter(o.from),b=socketCenter(o.to);if(a&&b)o.e={ax:a.x,ay:a.y,bx:b.x,by:b.y};}renderWires();}
export function applyEdgeColor(o,col){o.color=col;const[r,g,b]=hexRgb(col);o.g.style.setProperty('--wire',col);o.g.style.setProperty('--wire-soft',`rgba(${r},${g},${b},.5)`);
  [o.from,o.to].forEach(sel=>{const pe=world.querySelector(`[data-port="${sel}"]`);if(pe){pe.style.setProperty('--wire',col);pe.style.setProperty('--wire-soft',`rgba(${r},${g},${b},.5)`);}});
  const idx=GRAPH.edges.findIndex(e=>e.from===o.from&&e.to===o.to);if(idx>=0)GRAPH.edges[idx].color=col;renderMinimap();}
export function findWireFrom(sel){return wires.find(o=>o.from===sel);}
export function findWireTo(sel){return wires.find(o=>o.to===sel);}
export function addWire(e){const a=socketCenter(e.from),b=socketCenter(e.to);if(!a||!b)return null;
  const g=NS('g');g.setAttribute('class','wire');const hit=NS('path');hit.setAttribute('class','wire-hit');const base=NS('path');base.setAttribute('class','wire-base');const flow=NS('path');flow.setAttribute('class','wire-flow');g.appendChild(hit);g.appendChild(base);g.appendChild(flow);
  const sparks=[];for(let k=0;k<3;k++){const cc=NS('circle');cc.setAttribute('class','spark');cc.setAttribute('r','3');cc.setAttribute('fill','#EAFEFF');cc.setAttribute('opacity','0');g.appendChild(cc);sparks.push({el:cc,p:k/3});}
  svg.appendChild(g);const mx=(a.x+b.x)/2,my=(a.y+b.y)/2,o={g,hit,base,flow,sparks,from:e.from,to:e.to,i:wires.length,e:{ax:a.x,ay:a.y,bx:b.x,by:b.y}};hit.addEventListener('pointerdown',ev=>{ev.stopPropagation();selectWire(o);});
  const sag=restSag(o);o.sp={x:mx,y:my+sag};o.vel={x:0,y:0};o.off={x:0,y:sag};o.pm={x:mx,y:my};o.base.setAttribute('d',pathOf(o));o.flow.setAttribute('d',pathOf(o));o.hit.setAttribute('d',pathOf(o));
  wires.push(o);wireByEdge[e.from+'>'+e.to]=o;applyEdgeColor(o,e.color||getComputedStyle(document.documentElement).getPropertyValue('--wire').trim()||'#34E4EA');return o;}
// Idempotent so a graph load and the boot path can both call it: an
// edge that already has its wire (wireByEdge) is not built twice.
export function buildWires(){GRAPH.edges.forEach(e=>{if(!wireByEdge[e.from+'>'+e.to])addWire(e);});renderWires();}
