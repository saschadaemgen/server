// Minimap: the overview thumbnail (top-right) plus the viewport
// rectangle that tracks the current pan/zoom. renderMinimap rebuilds
// the node/edge thumbnails; updateMinimap just moves the viewport box.

import { GRAPH, nodes, S, NS } from './store.js';
import { graphBounds } from './transform.js';

const mmEdges=document.getElementById('mm-edges'),mmNodesG=document.getElementById('mm-nodes'),mmVp=document.getElementById('mm-vp');
const mmW=188,mmH=120;let mmS=1,mmB=null,mmOx=0,mmOy=0,mmPadX=16,mmPadTop=26,mmPadBot=12;
export function renderMinimap(){
  const bd=graphBounds();const gw=bd.maxX-bd.minX,gh=bd.maxY-bd.minY;
  if(!isFinite(gw)||!isFinite(gh)||gw<=0||gh<=0)return;
  mmB=bd;const padX=mmPadX,padTop=mmPadTop,padBot=mmPadBot,availW=mmW-padX*2,availH=mmH-padTop-padBot;
  mmS=Math.min(availW/gw,availH/gh,0.2);
  mmOx=padX+(availW-gw*mmS)/2;mmOy=padTop+(availH-gh*mmS)/2;
  const mx=x=>mmOx+(x-bd.minX)*mmS,my=y=>mmOy+(y-bd.minY)*mmS;
  mmEdges.innerHTML='';mmNodesG.innerHTML='';
  GRAPH.edges.forEach(e=>{const s=nodes[e.from.split(':')[0]],t=nodes[e.to.split(':')[0]];if(!s||!t)return;
    const ln=NS('line');ln.setAttribute('class','mm-edge');ln.setAttribute('x1',mx(s.el.offsetLeft+s.el.offsetWidth));ln.setAttribute('y1',my(s.el.offsetTop+s.el.offsetHeight/2));ln.setAttribute('x2',mx(t.el.offsetLeft));ln.setAttribute('y2',my(t.el.offsetTop+t.el.offsetHeight/2));mmEdges.appendChild(ln);});
  for(const id in nodes){const el=nodes[id].el,col=nodes[id].def.color;const rc=NS('rect');rc.setAttribute('class','mm-node');rc.style.setProperty('--mmc',col);
    rc.setAttribute('x',mx(el.offsetLeft));rc.setAttribute('y',my(el.offsetTop));rc.setAttribute('width',Math.max(6,el.offsetWidth*mmS));rc.setAttribute('height',Math.max(4,el.offsetHeight*mmS));rc.setAttribute('rx','2.5');rc.setAttribute('fill',col);mmNodesG.appendChild(rc);}
}
export function updateMinimap(){if(!mmB)return;const bd=mmB,s=mmS,cl=(v,a,b)=>Math.max(a,Math.min(b,v));
  const x0=(-S.tx)/S.scale,y0=(-S.ty)/S.scale,x1=(innerWidth-S.tx)/S.scale,y1=(innerHeight-S.ty)/S.scale;
  const L0=mmPadX,R0=mmW-mmPadX,T0=mmPadTop,B0=mmH-mmPadBot;
  const L=mmOx+(x0-bd.minX)*s,T=mmOy+(y0-bd.minY)*s,R=mmOx+(x1-bd.minX)*s,B=mmOy+(y1-bd.minY)*s;
  const l=cl(L,L0,R0),t=cl(T,T0,B0),r=cl(R,L0,R0),b=cl(B,T0,B0);
  mmVp.style.left=l+'px';mmVp.style.top=t+'px';mmVp.style.width=Math.max(8,r-l)+'px';mmVp.style.height=Math.max(6,b-t)+'px';}
