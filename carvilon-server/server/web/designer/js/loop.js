// Animation loop: per-frame wire wobble + travelling sparks (and the
// background repaint). Started from boot via requestAnimationFrame.

import { wires, reduceMotion } from './store.js';
import { stepPhysics, renderWires } from './wires.js';
import { drawBG } from './background.js';

export function tick(now){
  const boost=document.body.classList.contains('signals-on');
  if(!reduceMotion)drawBG();
  for(const o of wires)stepPhysics(o);
  renderWires();
  for(const o of wires){const L=o.base.getTotalLength();for(const s of o.sparks){
    if(!reduceMotion&&boost){s.p+=0.0052;if(s.p>1)s.p-=1;}
    const pp=s.p,pt=o.base.getPointAtLength(pp*L);s.el.setAttribute('cx',pt.x);s.el.setAttribute('cy',pt.y);
    const edge=pp<0.05?pp/0.05:(pp>0.95?(1-pp)/0.05:1);s.el.setAttribute('opacity',(reduceMotion||!boost)?0:edge);}}
  requestAnimationFrame(tick);
}
