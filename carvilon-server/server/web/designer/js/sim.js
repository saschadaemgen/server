// On-card controls and the little local demo "simulation": pressing
// the push-button pulses the wires and lights the lamp for the
// staircase hold time. This is editor-only eye-candy; the real engine
// feeds are a later ticket.

import { nodes, wireByEdge, reduceMotion, NS, world } from './store.js';
import { isRunning, pressNode, toggleNode } from './run.js';

let stairTimer=null,progRAF=null;
export function setSlider(id,v){const n=nodes[id].def;n.value=v;const el=nodes[id].el;const r=el.querySelector('input[data-act=slider]');if(r)r.value=v;const vv=el.querySelector('[data-vval]');if(vv)vv.textContent=v.toFixed(1)+' '+n.unit;}
export function setNodeOn(id,on,src){const ln=nodes[id];if(!ln)return;ln.def.on=on;ln.def.src=src;ln.el.classList.toggle('lit',on);const ctl=ln.el.querySelector('.node-ctl');if(ctl&&ctl.querySelector('.sw-track')){ctl.classList.toggle('on',on);const l=ctl.querySelector('.swlab');if(l)l.textContent=on?'On':'Off';}}
function flashNode(id){const ln=nodes[id];if(!ln)return;ln.el.classList.add('lit');setTimeout(()=>{if(nodes[id]&&!nodes[id].def.on)ln.el.classList.remove('lit');},320);}
export function firePulse(edgeKey,done){const o=wireByEdge[edgeKey];if(!o||reduceMotion){done&&done();return;}let L;try{L=o.base.getTotalLength();}catch(_){done&&done();return;}if(!L){done&&done();return;}
  const c=NS('circle');c.setAttribute('class','pulse');c.setAttribute('r','5');c.setAttribute('fill','#fff');const p0=o.base.getPointAtLength(0);c.setAttribute('cx',p0.x);c.setAttribute('cy',p0.y);o.g.appendChild(c);
  const t0=performance.now(),dur=360;let raf;const step=now=>{const p=Math.min(1,(now-t0)/dur);const pt=o.base.getPointAtLength(p*L);c.setAttribute('cx',pt.x);c.setAttribute('cy',pt.y);if(p<1)raf=requestAnimationFrame(step);else{c.remove();done&&done();}};raf=requestAnimationFrame(step);
  setTimeout(()=>{cancelAnimationFrame(raf);if(c.parentNode)c.remove();},dur+500);}
function startCountdown(sec){const bar=nodes['stair1']&&nodes['stair1'].el.querySelector('[data-prog]');if(!bar)return;const t0=performance.now(),dur=sec*1000;cancelAnimationFrame(progRAF);
  (function step(now){const k=Math.min(1,(now-t0)/dur);bar.style.width=(100-k*100)+'%';if(k<1)progRAF=requestAnimationFrame(step);})(t0);}
function fireTaster(){if(!nodes['lamp1']||!nodes['stair1'])return;
  setNodeOn('lamp1',true,'sim');
  firePulse('btn1:out>stair1:trig',()=>firePulse('stair1:q>lamp1:set'));
  const sec=nodes['stair1'].def.value;startCountdown(sec);
  clearTimeout(stairTimer);stairTimer=setTimeout(()=>{if(nodes['lamp1']&&nodes['lamp1'].def.src==='sim')setNodeOn('lamp1',false,'sim');const bar=nodes['stair1']&&nodes['stair1'].el.querySelector('[data-prog]');if(bar)bar.style.width='0%';},sec*1000);
}
world.addEventListener('click',e=>{const act=e.target.closest('[data-act]');if(!act)return;const t=act.dataset.act,id=act.closest('.node').dataset.id;
  // While a graph is running, on-card controls drive the REAL engine
  // (input endpoint) instead of the client-side demo simulation.
  if(t==='press'){if(isRunning())pressNode(id);else if(id==='btn1')fireTaster();else flashNode(id);}
  else if(t==='switch'){const on=!nodes[id].def.on;if(isRunning()){setNodeOn(id,on,'engine');toggleNode(id,on);}else setNodeOn(id,on,'manual');}});
world.addEventListener('input',e=>{const r=e.target.closest('input[data-act=slider]');if(!r)return;const id=r.closest('.node').dataset.id;setSlider(id,parseFloat(r.value));});
