// Nodes: building the visual node cards, the per-category default
// shapes for dropped blocks, the library drag-ghost / drop-to-create
// flow, and node deletion. The three demo nodes (button → staircase →
// lamp) are built from GRAPH when this module evaluates.

import { CAT, GRAPH, nodes, wires, wireByEdge, world, dragghost, S, snap, reduceMotion, selection } from './store.js';
import { selectOnly, clearSel } from './selection.js';
import { renderMinimap } from './minimap.js';
import { recomputeEndpoints } from './wires.js';
import { NAME_ICON, NAME_CAT, NAME_TYPE, NAME_CHANNEL, NAME_UNIT } from './palette.js';

let idc=0;

const CATALOG={
 'Taster':{cat:'input',icon:'circle-dot',title:'Taster',props:[{k:'Eingang',v:'I?',accent:true}],ports:{in:[],out:['Q']},control:'press'},
 'Bewegungsmelder':{cat:'input',icon:'radar',title:'Bewegungsmelder',props:[{k:'Eingang',v:'I?',accent:true}],ports:{in:[],out:['Q']},control:'switch',on:false},
 'Schalter':{cat:'input',icon:'toggle-left',title:'Schalter',props:[{k:'Eingang',v:'I?',accent:true}],ports:{in:[],out:['Q']},control:'switch',on:false},
 'UND':{cat:'logic',icon:'ampersand',title:'UND',props:[],ports:{in:['A','B'],out:['Q']}},
 'ODER':{cat:'logic',icon:'git-merge',title:'ODER',props:[],ports:{in:['A','B'],out:['Q']}},
 'NICHT':{cat:'logic',icon:'slash',title:'NICHT',props:[],ports:{in:['A'],out:['Q']}},
 'Treppenhauslicht':{cat:'time',icon:'timer',title:'Treppenhauslicht',props:[{k:'Modus',v:'Impuls'}],ports:{in:['Tr'],out:['Q']},control:'slider',value:3,min:1,max:10,step:.5,unit:'s',vlabel:'Haltezeit'},
 'Einschaltverzög.':{cat:'time',icon:'timer-reset',title:'Einschaltverzögerung',props:[{k:'Modus',v:'EIN'}],ports:{in:['Tr'],out:['Q']},control:'slider',value:2,min:.5,max:30,step:.5,unit:'s',vlabel:'Verzögerung'},
 'Impulsgeber':{cat:'time',icon:'activity',title:'Impulsgeber',props:[],ports:{in:['Tr'],out:['Q']},control:'slider',value:1,min:.1,max:5,step:.1,unit:'s',vlabel:'Impuls'},
 'Merker':{cat:'memory',icon:'bookmark',title:'Merker',props:[{k:'Wert',v:'0'}],ports:{in:['S','R'],out:['Q']},control:'switch',on:false},
 'Statusbaustein':{cat:'memory',icon:'database',title:'Statusbaustein',props:[{k:'Status',v:'—'}],ports:{in:['I'],out:['Q']}},
 'Lampe':{cat:'output',icon:'lightbulb',title:'Lampe',props:[{k:'Ausgang',v:'Q?',accent:true},{k:'Kanal',v:'DALI 1'}],ports:{in:['AI'],out:[]},control:'switch',on:false},
 'Relais':{cat:'output',icon:'toggle-right',title:'Relais',props:[{k:'Ausgang',v:'Q?',accent:true}],ports:{in:['AI'],out:[]},control:'switch',on:false},
 'Jalousie':{cat:'output',icon:'blinds',title:'Jalousie',props:[{k:'Ausgang',v:'Q?',accent:true}],ports:{in:['Auf','Ab'],out:[]},control:'slider',value:50,min:0,max:100,step:5,unit:'%',vlabel:'Position'}
};
function ctlHTML(n){
  if(n.control==='press') return `<div class="node-ctl" data-noselectdrag><button class="ctl-press" data-act="press"><i data-lucide="hand"></i>Press</button></div>`;
  if(n.control==='switch') return `<div class="node-ctl" data-noselectdrag data-act="switch"><div class="ctl-switch"><span class="swlab">Off</span><span class="sw-track"><span class="sw-thumb"></span></span></div></div>`;
  if(n.control==='slider') return `<div class="node-ctl" data-noselectdrag><div class="ctl-slider"><div class="slhead"><span class="k">${n.vlabel}</span><span class="v" data-vval>${n.value.toFixed(1)} ${n.unit}</span></div><input type="range" min="${n.min}" max="${n.max}" step="${n.step}" value="${n.value}" data-act="slider"><div class="ctl-prog"><i data-prog></i></div></div></div>`;
  return '';
}
// pvDisp renders a prop's card value: enums show their friendly label
// (not the raw 'pullup'), everything else its value verbatim.
function pvDisp(p){return p.opts?((p.opts.find(o=>o.v===p.v)||{}).l||p.v):p.v;}
function renderNodeBody(n){const rows=n.props.filter(p=>!p.inspectorOnly).map(p=>`<div class="prow"><span class="pk">${p.k}</span><span class="pv ${p.accent?'accent':''}">${pvDisp(p)}</span></div>`).join('');return rows+ctlHTML(n);}
function normPorts(n){n.ports.in=(n.ports.in||[]).map(p=>typeof p==='string'?{id:p,label:p}:p);n.ports.out=(n.ports.out||[]).map(p=>typeof p==='string'?{id:p,label:p}:p);}
export function buildNode(n){
  if(n.color==null)n.color=CAT[n.cat].color;normPorts(n);
  const c=CAT[n.cat],el=document.createElement('div');
  el.className='node';el.dataset.id=n.id;el.style.left=n.ui.x+'px';el.style.top=n.ui.y+'px';el.style.setProperty('--cat',n.color);
  const insH=n.ports.in.map(p=>`<div class="port" data-port="${n.id}:${p.id}" data-tip="${p.label} · Input"><span class="socket"></span></div>`).join('');
  const outH=n.ports.out.map(p=>`<div class="port" data-port="${n.id}:${p.id}" data-tip="${p.label} · Output"><span class="socket"></span></div>`).join('');
  el.innerHTML=`<div class="node-accent"></div>
    <div class="node-head"><div class="node-icon"><i data-lucide="${n.icon}"></i></div>
    <div class="node-titles"><div class="node-cat" data-catlabel>${c.label}</div><div class="node-title" data-titletext>${n.title}</div></div></div>
    <div class="node-live" data-live></div>
    <div class="node-body" data-body>${renderNodeBody(n)}</div>
    ${insH?`<div class="ports ports-in">${insH}</div>`:''}${outH?`<div class="ports ports-out">${outH}</div>`:''}`;
  // idle live-value placeholder via textContent (never innerHTML) so a unit
  // is inert markup-wise regardless of where it came from.
  if(n.live)el.querySelector('[data-live]').textContent=n.unit?('— '+n.unit):'—';
  world.appendChild(el);nodes[n.id]={def:n,el};
  if(window.lucide)lucide.createIcons();return el;
}
GRAPH.nodes.forEach(buildNode);
function defFor(name,cat){
  // GPIO blocks (and any future engine-backed I/O block) are typed to the
  // engine's source.channel / sink.channel nodes with their engine port
  // names and an editable "Line" prop that serializes as the channel param
  // (a physical ref like gpio:gpiochip0:17). Only these are typed for Run;
  // the other library blocks stay inert (general node-typing is a follow-up).
  const t=NAME_TYPE[name];
  // MQTT blocks: free-text Topic + a value-type selector that re-types
  // the node (source/sink.channel.<kind>) and, on the sink, a Retain
  // switch. The Topic prop holds the raw topic; run.js prefixes "mqtt:"
  // when it serializes the channel param. Seed type is the float variant
  // (the catalog's), matching the default value-type "float".
  if(NAME_CAT[name]==='mqtt'){
    const isSrc=t.indexOf('source')===0,base=isSrc?'source.channel':'sink.channel';
    const topic={k:'Topic',v:'',param:'channel',kind:'mqtt-topic'};
    const kindp={k:'Werttyp',v:'float',kind:'mqtt-kind',base,
      opts:[{v:'bool',l:'Bool (an/aus)'},{v:'float',l:'Float (Zahl)'},{v:'text',l:'Text'}]};
    const props=isSrc?[topic,kindp]
      :[topic,kindp,{k:'Retain',v:'false',param:'retain',kind:'enum',opts:[{v:'false',l:'Aus'},{v:'true',l:'Ein'}]}];
    return {cat:'mqtt',icon:NAME_ICON[name]||'radio',title:name,type:t,implemented:true,live:isSrc,props,
      ports:isSrc?{in:[],out:[{id:'out',label:'OUT'}]}:{in:[{id:'in',label:'IN'}],out:[]}};
  }
  if(t==='source.channel'||t==='sink.channel'){const isSrc=t==='source.channel',gc=NAME_CAT[name]||'gpio';
    // Like the other blocks (staircase shows Mode/Hold, lamp shows
    // Output/Channel), the GPIO card shows its pin options; they stay
    // editable in the inspector. Defaults match the driver's prior fixed
    // behaviour (input: pull-up + active-low; output: active-high, initial
    // low) so an untouched block does not regress.
    const line={k:'Line',v:'',param:'channel',kind:'gpio-line'};
    const props=isSrc?[line,
      {k:'Bias',param:'bias',kind:'enum',v:'pullup',opts:[{v:'pullup',l:'Pull-up'},{v:'pulldown',l:'Pull-down'},{v:'none',l:'Kein'}]},
      {k:'Pegel',param:'active_level',kind:'enum',v:'low',opts:[{v:'low',l:'Active-Low'},{v:'high',l:'Active-High'}]},
      {k:'Entprellung',param:'debounce_ms',kind:'number',v:'0',suffix:'ms'}]
    :[line,
      {k:'Startwert',param:'initial',kind:'enum',v:'low',opts:[{v:'low',l:'Low'},{v:'high',l:'High'}]},
      {k:'Pegel',param:'active_level',kind:'enum',v:'high',opts:[{v:'high',l:'Active-High'},{v:'low',l:'Active-Low'}]}];
    return {cat:gc,icon:NAME_ICON[name]||(CAT[gc]&&CAT[gc].icon)||'cpu',title:name,type:t,implemented:true,props,
      ports:isSrc?{in:[],out:[{id:'out',label:'OUT'}]}:{in:[{id:'in',label:'IN'}],out:[]}};}
  if(t==='source.channel.float'||t==='source.channel.text'||t==='sink.channel.float'||t==='sink.channel.text'){
    // Float/Text channel blocks (e.g. system telemetry): the channel is
    // fixed by the catalog (no picker), kept as an inspector-only param so
    // it serializes; the card shows the live value (with its unit) in a run.
    const isSrc=t.indexOf('source')===0,gc=NAME_CAT[name]||'system',unit=NAME_UNIT[name]||'';
    return {cat:gc,icon:NAME_ICON[name]||(CAT[gc]&&CAT[gc].icon)||'gauge',title:name,type:t,implemented:true,unit,live:true,
      props:[{k:'Kanal',v:NAME_CHANNEL[name]||'',param:'channel',inspectorOnly:true}],
      ports:isSrc?{in:[],out:[{id:'out',label:'OUT'}]}:{in:[{id:'in',label:'IN'}],out:[]}};}
  if(CATALOG[name])return CATALOG[name];const c=cat||NAME_CAT[name]||'logic',icon=NAME_ICON[name]||CAT[c].icon,base={cat:c,icon,title:name};
  if(c==='input')return{...base,props:[{k:'Input',v:'I?',accent:true}],ports:{in:[],out:['Q']},control:'switch',on:false};
  if(c==='logic')return{...base,props:[],ports:{in:['A','B'],out:['Q']}};
  if(c==='time')return{...base,props:[{k:'Mode',v:'—'}],ports:{in:['Tr'],out:['Q']},control:'slider',value:3,min:.5,max:30,step:.5,unit:'s',vlabel:'Time'};
  if(c==='memory')return{...base,props:[{k:'Value',v:'0'}],ports:{in:['S','R'],out:['Q']},control:'switch',on:false};
  if(c==='output')return{...base,props:[{k:'Output',v:'Q?',accent:true}],ports:{in:['AI'],out:[]},control:'switch',on:false};
  return{...base,props:[],ports:{in:['A'],out:['Q']}};}
function createNode(name,wx,wy,cat){const t=defFor(name,cat);if(!t)return;
  const def=JSON.parse(JSON.stringify(t));def.id=name.toLowerCase().replace(/[^a-z0-9]/g,'')+'_'+(++idc);def.title=def.title||name;def.ui={x:snap(wx-107),y:snap(wy-40)};
  GRAPH.nodes.push(def);const nel=buildNode(def);if(!reduceMotion){nel.classList.add('spawn');fxBurst(def.ui.x,def.ui.y,nel.offsetWidth,nel.offsetHeight,def.color);}selectOnly(def.id);renderMinimap();}

/* ===== create (drag from library) + delete ===== */
export function attachDrag(it){
  it.addEventListener('pointerdown',ev=>{if(it.classList.contains('inactive')||ev.target.closest('.fav-x'))return;ev.preventDefault();const name=it.dataset.name,cat=it.dataset.cat;S.newDrag={name,cat};
    dragghost.style.setProperty('--gc',CAT[cat].color);dragghost.innerHTML=`<span class="gi"><i data-lucide="${NAME_ICON[name]||CAT[cat].icon}"></i></span>${name}`;
    if(window.lucide)lucide.createIcons();dragghost.classList.add('show');moveGhost(ev);
    try{it.setPointerCapture(ev.pointerId);}catch(_){}
    it._mv=e2=>moveGhost(e2);it._up=e2=>dropNew(e2,it);
    it.addEventListener('pointermove',it._mv);it.addEventListener('pointerup',it._up);});
}
export function moveGhost(e){dragghost.style.left=(e.clientX+14)+'px';dragghost.style.top=(e.clientY+10)+'px';}
export function dropNew(e,it){it.removeEventListener('pointermove',it._mv);it.removeEventListener('pointerup',it._up);dragghost.classList.remove('show');
  const nd=S.newDrag;S.newDrag=null;if(!nd)return;const el=document.elementFromPoint(e.clientX,e.clientY);
  if(el&&el.closest('#viewport')&&!el.closest('.rail,.inspector,.topbar,.minimap,.zoom,.alignbar')){S.userAdjusted=true;const wx=(e.clientX-S.tx)/S.scale,wy=(e.clientY-S.ty)/S.scale;createNode(nd.name,wx,wy,nd.cat);}}
function fxBurst(x,y,w,h,color){const wrap=document.createElement('div');wrap.className='fx-wrap';wrap.style.left=x+'px';wrap.style.top=y+'px';wrap.style.width=w+'px';wrap.style.height=h+'px';wrap.style.setProperty('--cat',color);
  const ring=document.createElement('div');ring.className='fx-ring';wrap.appendChild(ring);
  for(let i=0;i<12;i++){const p=document.createElement('span');p.className='fx-part';const a=Math.random()*6.283,d=42+Math.random()*60;p.style.setProperty('--dx',Math.cos(a)*d+'px');p.style.setProperty('--dy',Math.sin(a)*d+'px');p.style.animationDelay=(Math.random()*.06)+'s';wrap.appendChild(p);}
  world.appendChild(wrap);setTimeout(()=>wrap.remove(),720);}
function deleteNode(id){const n=nodes[id];if(!n)return;const el=n.el,col=n.def.color,x=el.offsetLeft,y=el.offsetTop,w=el.offsetWidth,h=el.offsetHeight;
  const rm=wires.filter(o=>o.from.split(':')[0]===id||o.to.split(':')[0]===id);
  rm.forEach(o=>{o.g.remove();const wi=wires.indexOf(o);if(wi>=0)wires.splice(wi,1);delete wireByEdge[o.from+'>'+o.to];});
  for(let i=GRAPH.edges.length-1;i>=0;i--){const e=GRAPH.edges[i];if(e.from.split(':')[0]===id||e.to.split(':')[0]===id)GRAPH.edges.splice(i,1);}
  delete nodes[id];const gi=GRAPH.nodes.findIndex(x=>x.id===id);if(gi>=0)GRAPH.nodes.splice(gi,1);
  if(reduceMotion){el.remove();}else{fxBurst(x,y,w,h,col);el.classList.add('despawn');setTimeout(()=>el.remove(),340);}}
export function deleteSelected(){if(!selection.size)return;[...selection].forEach(deleteNode);clearSel();renderMinimap();recomputeEndpoints();}
