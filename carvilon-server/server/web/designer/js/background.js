// Background: the static, world-locked dot grid plus the slow drifting
// colour blobs painted on the #bg canvas. sizeCanvas tracks the
// viewport/DPR; drawBG repaints one frame.
//
// The grid also carries a very subtle "Starlink train": every now and
// then (a few times per minute) a short chain of grid dots lights up and
// glides across the grid in a straight line — horizontal, vertical or
// diagonal — then fades. It is faint eye-candy, demo-only, configurable
// via the toolbar settings (FX), and skipped under reduced motion
// (drawBG isn't called then).

import { S, reduceMotion, FX } from './store.js';

const cv=document.getElementById('bg'),bctx=cv.getContext('2d');let DPR=Math.min(2,devicePixelRatio||1);
const blobs=[{x:.62,y:.24,r:460,c:'59,130,246',a:.07,ph:0},{x:.18,y:.82,r:380,c:'52,228,234',a:.05,ph:2.1},{x:.86,y:.66,r:320,c:'120,90,240',a:.045,ph:4.2},{x:.45,y:.52,r:300,c:'40,110,200',a:.035,ph:1.2}];
const motes=[];for(let i=0;i<14;i++)motes.push({x:Math.random(),y:Math.random(),r:.6+Math.random()*1.3,sp:.004+Math.random()*.011});

// ---- "Starlink train" grid FX ----
// A train is a chain of grid cells (in world-grid indices) that the head
// walks along one of 8 grid-aligned directions; the trail fades behind it
// like a string of satellites. Trains are rare (FX.rate per minute) and
// faint (FX.intensity). State lives on gridFX; FX (store.js) holds the
// live, persisted tuning.
const gridFX={trains:[],nextSpawn:0,lastT:0,tw:new Map(),twNext:0};
const BLUES=['96,200,255','130,222,255','64,162,255','120,150,255','43,217,255'];
const SOFT=['200,228,255','160,205,255','120,150,255'];
const DIRS=[[1,0],[-1,0],[0,1],[0,-1],[1,1],[1,-1],[-1,1],[-1,-1]];
function trainColor(){return Math.random()<0.85?BLUES[Math.random()*BLUES.length|0]:SOFT[Math.random()*SOFT.length|0];}
// accentRGB returns the CSS --accent colour as an "r,g,b" string (cached;
// re-parsed only when the variable changes), used for the tick flash.
let _accRaw='',_accRGB='59,130,246';
function accentRGB(){try{const raw=getComputedStyle(document.documentElement).getPropertyValue('--accent').trim();if(raw&&raw!==_accRaw){_accRaw=raw;let h=raw.replace('#','');if(h.length===3)h=h.split('').map(c=>c+c).join('');if(/^[0-9a-f]{6}$/i.test(h))_accRGB=parseInt(h.slice(0,2),16)+','+parseInt(h.slice(2,4),16)+','+parseInt(h.slice(4,6),16);}}catch(e){}return _accRGB;}
function scheduleNext(now){const base=60000/Math.max(0.2,FX.rate);gridFX.nextSpawn=now+base*(0.55+Math.random()*0.9);}
function gridRange(){const cell=S.grid*S.scale;return{cell,gxMin:Math.floor(-S.tx/cell)-1,gxMax:Math.ceil((innerWidth-S.tx)/cell)+1,gyMin:Math.floor(-S.ty/cell)-1,gyMax:Math.ceil((innerHeight-S.ty)/cell)+1};}
// spawnTrain places the head just outside the edge it enters from (random
// position along that edge) so it appears promptly and glides across — no
// long off-screen lead-in regardless of speed/zoom.
function spawnTrain(r){const[dx,dy]=DIRS[Math.random()*DIRS.length|0];const m=3;
  const rx=()=>r.gxMin+Math.floor(Math.random()*(r.gxMax-r.gxMin+1)),ry=()=>r.gyMin+Math.floor(Math.random()*(r.gyMax-r.gyMin+1));
  let gx,gy;
  if(dx&&dy){ // diagonal: enter from a vertical or horizontal edge
    if(Math.random()<0.5){gx=dx>0?r.gxMin-m:r.gxMax+m;gy=ry();}
    else{gy=dy>0?r.gyMin-m:r.gyMax+m;gx=rx();}
  }else{ // axis-aligned: just outside the entering edge, random cross-axis
    gx=dx>0?r.gxMin-m:dx<0?r.gxMax+m:rx();
    gy=dy>0?r.gyMin-m:dy<0?r.gyMax+m:ry();
  }
  const len=Math.max(3,Math.round(FX.length));
  gridFX.trains.push({gx,gy,dx,dy,prog:0,trail:[[gx,gy,0]],len,speed:FX.speed*(0.8+Math.random()*0.5),color:trainColor()});}
function updateTrains(now){
  const dt=gridFX.lastT?Math.min(0.05,(now-gridFX.lastT)/1000):0;gridFX.lastT=now;
  if(!FX.enabled){if(gridFX.trains.length)gridFX.trains.length=0;scheduleNext(now);return;}
  if(!gridFX.nextSpawn)scheduleNext(now);
  // keep the pending wait in sync with the live rate: if Häufigkeit was just
  // raised, shorten an over-long scheduled wait so it takes effect at once.
  const base=60000/Math.max(0.2,FX.rate);if(gridFX.nextSpawn>now+base*1.6)gridFX.nextSpawn=now+base*(0.3+Math.random()*0.7);
  const r=gridRange();if(r.cell<=0)return;
  if(now>=gridFX.nextSpawn){if(gridFX.trains.length<6)spawnTrain(r);scheduleNext(now);}
  for(const t of gridFX.trains){t.prog+=t.speed*dt;let guard=0;
    while(t.prog>=1&&guard++<8){t.prog-=1;t.gx+=t.dx;t.gy+=t.dy;const tick=FX.tickEnabled&&Math.random()<FX.tickRate;t.trail.unshift([t.gx,t.gy,tick?1:0]);if(t.trail.length>t.len)t.trail.pop();}}
  const mx=Math.max(r.gxMax-r.gxMin,r.gyMax-r.gyMin)+10;
  gridFX.trains=gridFX.trains.filter(t=>t.trail.length&&t.gx>=r.gxMin-mx&&t.gx<=r.gxMax+mx&&t.gy>=r.gyMin-mx&&t.gy<=r.gyMax+mx);}
// Ambient twinkle: the constant gentle "funkeln" — every ~130 ms a few
// random grid dots light up and fade in/out like little stars (sin pulse).
// Sparse by default; FX.twinkleRate scales how many per beat.
function updateTwinkle(now,r){
  if(!FX.twinkle){if(gridFX.tw.size)gridFX.tw.clear();gridFX.twNext=now;return;}
  if(!gridFX.twNext)gridFX.twNext=now;let n=0;
  while(now>=gridFX.twNext&&n<5){gridFX.twNext+=130;n++;
    const cnt=Math.round((0.4+Math.random()*1.1)*FX.twinkleRate);
    for(let i=0;i<cnt;i++){const gx=r.gxMin+(Math.random()*(r.gxMax-r.gxMin+1)|0),gy=r.gyMin+(Math.random()*(r.gyMax-r.gyMin+1)|0);gridFX.tw.set(gx+','+gy,{t0:now,color:trainColor(),life:560+Math.random()*460});}}
  if(now-gridFX.twNext>520)gridFX.twNext=now;
  for(const[k,s]of gridFX.tw)if(now-s.t0>=s.life)gridFX.tw.delete(k);}

export function sizeCanvas(){DPR=Math.min(2,devicePixelRatio||1);cv.width=innerWidth*DPR;cv.height=innerHeight*DPR;cv.style.width=innerWidth+'px';cv.style.height=innerHeight+'px';}
export function drawBG(){const W=cv.width,H=cv.height,T=reduceMotion?0:performance.now()/1000;bctx.clearRect(0,0,W,H);
  bctx.globalCompositeOperation='lighter';for(const bl of blobs){const px=(bl.x*innerWidth+Math.sin(T*0.06+bl.ph)*46)*DPR,py=(bl.y*innerHeight+Math.cos(T*0.05+bl.ph)*34)*DPR,g=bctx.createRadialGradient(px,py,0,px,py,bl.r*DPR);g.addColorStop(0,`rgba(${bl.c},${bl.a})`);g.addColorStop(1,`rgba(${bl.c},0)`);bctx.fillStyle=g;bctx.fillRect(0,0,W,H);}
  bctx.globalCompositeOperation='source-over';
  if(!S.gridOn)return;
  const Sg=S.grid*S.scale*DPR,ox=(S.tx*DPR)%Sg,oy=(S.ty*DPR)%Sg,cx=innerWidth/2*DPR,cy=(innerHeight/2+20)*DPR,maxD=Math.hypot(cx,cy);
  bctx.fillStyle='#9fc3d2';
  for(let x=(ox%Sg)-Sg;x<W+Sg;x+=Sg)for(let y=(oy%Sg)-Sg;y<H+Sg;y+=Sg){const d=Math.hypot(x-cx,y-cy)/maxD,a=Math.max(0,0.34-d*0.30);if(a<=0.012)continue;bctx.globalAlpha=a;bctx.beginPath();bctx.arc(x,y,1.0*DPR,0,6.283);bctx.fill();}
  bctx.globalAlpha=1;
  // FX layers (twinkle + Starlink trains): glow on a lighter blend
  const now=performance.now(),r=gridRange();updateTwinkle(now,r);updateTrains(now);
  const cell=S.grid*S.scale,inten=FX.intensity,blur=FX.blur,acc=accentRGB(),ti=FX.tickIntensity;
  if(gridFX.tw.size||gridFX.trains.length){bctx.save();bctx.globalCompositeOperation='lighter';
    // ambient twinkle (gentle sin in/out)
    for(const[k,s]of gridFX.tw){const c=k.indexOf(','),gx=+k.slice(0,c),gy=+k.slice(c+1),x=(gx*cell+S.tx)*DPR,y=(gy*cell+S.ty)*DPR;if(x<-30||x>W+30||y<-30||y>H+30)continue;
      const e=Math.sin(Math.min(1,(now-s.t0)/s.life)*Math.PI);if(e<=0)continue;
      bctx.globalAlpha=Math.min(1,0.42*e*inten);bctx.shadowColor='rgba('+s.color+',0.9)';bctx.shadowBlur=7*DPR*e*blur;bctx.fillStyle='rgba('+s.color+',1)';
      bctx.beginPath();bctx.arc(x,y,(0.7+1.3*e)*DPR,0,6.283);bctx.fill();}
    // trains: each passed dot flares bright then twinkles as it fades
    for(const t of gridFX.trains){const tl=t.trail,len=t.len;
      for(let i=0;i<tl.length;i++){const gx=tl[i][0],gy=tl[i][1],x=(gx*cell+S.tx)*DPR,y=(gy*cell+S.ty)*DPR;if(x<-30||x>W+30||y<-30||y>H+30)continue;
        const fade=1-i/len,head=i===0?1.5:1,e=0.4+0.6*fade,sh=0.72+0.28*Math.sin(now*0.013+gx*1.7+gy*0.9),tick=tl[i][2],col=tick?acc:t.color,ii=(tick?inten*ti:inten)*head*sh;
        bctx.globalAlpha=Math.min(1,0.62*e*ii);bctx.shadowColor='rgba('+col+',0.98)';bctx.shadowBlur=(tick?13:8)*DPR*e*blur;bctx.fillStyle='rgba('+col+',1)';
        bctx.beginPath();bctx.arc(x,y,(0.8+(tick?2.6:1.7)*e)*DPR,0,6.283);bctx.fill();}}
    bctx.restore();bctx.globalAlpha=1;}}
