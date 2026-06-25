// Background: the static, world-locked dot grid plus the slow drifting
// colour blobs painted on the #bg canvas. sizeCanvas tracks the
// viewport/DPR; drawBG repaints one frame.

import { S, GRID, reduceMotion } from './store.js';

const cv=document.getElementById('bg'),bctx=cv.getContext('2d');let DPR=Math.min(2,devicePixelRatio||1);
const blobs=[{x:.62,y:.24,r:460,c:'59,130,246',a:.07,ph:0},{x:.18,y:.82,r:380,c:'52,228,234',a:.05,ph:2.1},{x:.86,y:.66,r:320,c:'120,90,240',a:.045,ph:4.2},{x:.45,y:.52,r:300,c:'40,110,200',a:.035,ph:1.2}];
const motes=[];for(let i=0;i<14;i++)motes.push({x:Math.random(),y:Math.random(),r:.6+Math.random()*1.3,sp:.004+Math.random()*.011});
export function sizeCanvas(){DPR=Math.min(2,devicePixelRatio||1);cv.width=innerWidth*DPR;cv.height=innerHeight*DPR;cv.style.width=innerWidth+'px';cv.style.height=innerHeight+'px';}
export function drawBG(){const W=cv.width,H=cv.height,T=reduceMotion?0:performance.now()/1000;bctx.clearRect(0,0,W,H);
  bctx.globalCompositeOperation='lighter';for(const bl of blobs){const px=(bl.x*innerWidth+Math.sin(T*0.06+bl.ph)*46)*DPR,py=(bl.y*innerHeight+Math.cos(T*0.05+bl.ph)*34)*DPR,g=bctx.createRadialGradient(px,py,0,px,py,bl.r*DPR);g.addColorStop(0,`rgba(${bl.c},${bl.a})`);g.addColorStop(1,`rgba(${bl.c},0)`);bctx.fillStyle=g;bctx.fillRect(0,0,W,H);}
  bctx.globalCompositeOperation='source-over';
  if(!S.gridOn)return;
  const Sg=GRID*S.scale*DPR,ox=(S.tx*DPR)%Sg,oy=(S.ty*DPR)%Sg,cx=innerWidth/2*DPR,cy=(innerHeight/2+20)*DPR,maxD=Math.hypot(cx,cy);
  bctx.fillStyle='#9fc3d2';
  for(let x=(ox%Sg)-Sg;x<W+Sg;x+=Sg)for(let y=(oy%Sg)-Sg;y<H+Sg;y+=Sg){const d=Math.hypot(x-cx,y-cy)/maxD,a=Math.max(0,0.34-d*0.30);if(a<=0.012)continue;bctx.globalAlpha=a;bctx.beginPath();bctx.arc(x,y,1.0*DPR,0,6.283);bctx.fill();}
  bctx.globalAlpha=1;}
