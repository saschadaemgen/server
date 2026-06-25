// Transform: the world pan/zoom (S.scale/S.tx/S.ty live in the store).
// applyTransform pushes the current transform onto #world and repaints
// the grid + minimap; graphBounds is the node bounding box; fit frames
// the whole graph in the viewport.

import { S, nodes, world } from './store.js';
import { drawBG } from './background.js';
import { renderMinimap, updateMinimap } from './minimap.js';
import { recomputeEndpoints } from './wires.js';

export function applyTransform(){world.style.transform=`translate(${S.tx}px,${S.ty}px) scale(${S.scale})`;const _zv=document.getElementById('rt-zval');if(_zv)_zv.textContent=Math.round(S.scale*100)+'%';drawBG();updateMinimap();}
export function graphBounds(){let a=1e9,b=1e9,c=-1e9,d=-1e9;for(const id in nodes){const el=nodes[id].el,x=el.offsetLeft,y=el.offsetTop,w=el.offsetWidth,h=el.offsetHeight;a=Math.min(a,x);b=Math.min(b,y);c=Math.max(c,x+w);d=Math.max(d,y+h);}return{minX:a,minY:b,maxX:c,maxY:d};}
export function fit(an){const bd=graphBounds(),pad=150,vw=innerWidth,vh=innerHeight,gw=(bd.maxX-bd.minX)+pad*2,gh=(bd.maxY-bd.minY)+pad*2,rail=vw>980?236:0,rgt=vw>760?212:0;
  S.scale=Math.max(.4,Math.min(1.1,(vw-rail-rgt)/gw,(vh-90)/gh));
  S.tx=rail+(vw-rail-rgt)/2-(bd.minX+(bd.maxX-bd.minX)/2)*S.scale;S.ty=54+(vh-54)/2-(bd.minY+(bd.maxY-bd.minY)/2)*S.scale;
  if(an)world.style.transition='transform .5s cubic-bezier(.2,.7,.2,1)';applyTransform();recomputeEndpoints();renderMinimap();updateMinimap();if(an)setTimeout(()=>world.style.transition='',520);}
