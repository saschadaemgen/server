// Nodes: building the visual node cards, the per-category default
// shapes for dropped blocks, the library drag-ghost / drop-to-create
// flow, and node deletion. The canvas starts empty; project.js builds
// the persisted graph via buildNode once it arrives from the API.

import { CAT, GRAPH, nodes, wires, wireByEdge, world, dragghost, S, snap, reduceMotion, selection, markDirty, esc, escAttr } from './store.js';
import { selectOnly, clearSel } from './selection.js';
import { renderMinimap } from './minimap.js';
import { recomputeEndpoints } from './wires.js';
import { NAME_ICON, NAME_CAT, NAME_TYPE, NAME_CHANNEL, NAME_UNIT, NAME_INPUTS, NAME_OUTPUTS, NAME_SHELLY, NAME_READOUT, NAME_DESC } from './palette.js';

let idc=0;

// PORT_LABEL maps an engine port name to a short on-card label (Loxone
// style: inputs like Trg/Set/In, outputs like Q). Unknown names fall
// back to uppercase, so any future engine port stays legible.
const PORT_LABEL={out:'Q',in:'In',a:'A',b:'B',trig:'Trg',q:'Q',set:'Set',value:'Val'};
function portLabel(name){return PORT_LABEL[name]||String(name||'').toUpperCase();}
// kindOfType derives the value kind of a generic channel node's single
// port from its type suffix (.float/.text, else bool), so driver blocks
// carry a real kind for type-checked wiring and live-value colouring.
function kindOfType(t){
  if(t.slice(-6)==='.float')return 'float';
  if(t.slice(-5)==='.text')return 'text';
  if(t==='source.channel'||t==='sink.channel')return 'bool';
  return undefined;
}
// portsFromCatalog builds the {in,out} port lists for an implemented
// block from the catalog's real port metadata (name/kind/optional) - the
// single source of truth - so labels and kinds are never invented.
function portsFromCatalog(name){
  const mk=(p,isIn)=>({id:p.name,label:portLabel(p.name),kind:p.kind,optional:isIn?!!p.optional:undefined});
  return {in:(NAME_INPUTS[name]||[]).map(p=>mk(p,true)),out:(NAME_OUTPUTS[name]||[]).map(p=>mk(p,false))};
}
function ctlHTML(n){
  if(n.control==='press') return `<div class="node-ctl" data-noselectdrag><button class="ctl-press" data-act="press"><i data-lucide="hand"></i>Press</button></div>`;
  if(n.control==='switch') return `<div class="node-ctl" data-noselectdrag data-act="switch"><div class="ctl-switch"><span class="swlab">Off</span><span class="sw-track"><span class="sw-thumb"></span></span></div></div>`;
  if(n.control==='slider') return `<div class="node-ctl" data-noselectdrag><div class="ctl-slider"><div class="slhead"><span class="k">${esc(n.vlabel)}</span><span class="v" data-vval>${Number(n.value).toFixed(1)} ${esc(n.unit)}</span></div><input type="range" min="${Number(n.min)}" max="${Number(n.max)}" step="${Number(n.step)}" value="${Number(n.value)}" data-act="slider"><div class="ctl-prog"><i data-prog></i></div></div></div>`;
  return '';
}
// pvDisp renders a prop's card value: enums show their friendly label
// (not the raw 'pullup'), everything else its value verbatim.
function pvDisp(p){return p.opts?((p.opts.find(o=>o.v===p.v)||{}).l||p.v):p.v;}
function renderNodeBody(n){const rows=n.props.filter(p=>!p.inspectorOnly).map(p=>`<div class="prow"><span class="pk">${esc(p.k)}</span><span class="pv ${p.accent?'accent':''}">${esc(pvDisp(p))}</span></div>`).join('');return rows+ctlHTML(n);}
function normPorts(n){n.ports.in=(n.ports.in||[]).map(p=>typeof p==='string'?{id:p,label:p}:p);n.ports.out=(n.ports.out||[]).map(p=>typeof p==='string'?{id:p,label:p}:p);}
// portTip is the hover tooltip: label, direction and (when known) the
// value kind, so a port's contract is legible even before it is wired.
function portTip(label,dir,kind){return escAttr(label+' · '+dir+(kind?(' · '+kind):''));}
function kindClass(k){return k?(' k-'+k):'';}
// portRowHTML renders one input/output row: a socket on the card edge, a
// short label, and a live-value slot. Inputs read left-to-right, outputs
// mirror to the right edge (value, label, socket).
function portRowHTML(nodeId,p,dir){
  const sel=escAttr(nodeId+':'+p.id),tip=portTip(p.label,dir,p.kind),sock=`<span class="socket${kindClass(p.kind)}"></span>`;
  const name=`<span class="io-name">${esc(p.label)}</span>`,val=`<span class="io-val" data-pval></span>`;
  const cls='port io-row '+(dir==='Input'?'io-in':'io-out');
  return `<div class="${cls}" data-port="${sel}" data-tip="${tip}">${dir==='Input'?sock+name+val:val+name+sock}</div>`;
}
export function buildNode(n){
  // A stored graph can carry a category this host's catalog does not
  // expose (e.g. gpio saved on a Pi, opened elsewhere) - register a
  // neutral fallback instead of crashing the load.
  upgradeClimateDef(n);
  const c=CAT[n.cat]||(CAT[n.cat]={color:'#7f8c99',label:String(n.cat||'?').toUpperCase(),icon:'box'});
  if(n.color==null)n.color=c.color;normPorts(n);
  const el=document.createElement('div');
  el.className='node'+(n.faceplate?' node-shelly':'')+(n.readout?' node-readout':'')+(n.type==='midea.control_loop'?' node-climate-loop':'');el.dataset.id=n.id;el.style.left=n.ui.x+'px';el.style.top=n.ui.y+'px';el.style.setProperty('--cat',n.color);
  if(n.faceplate){
    el.innerHTML=`<div class="node-accent"></div>`+(n.type==='midea.control_loop'?climateFaceplateHTML(n):(n.readout?readoutFaceplateHTML(n):shellyFaceplateHTML(n)));
  }else{
    const insH=n.ports.in.map(p=>portRowHTML(n.id,p,'Input')).join('');
    const outH=n.ports.out.map(p=>portRowHTML(n.id,p,'Output')).join('');
    const ioH=(insH||outH)?`<div class="node-io"><div class="io-col io-col-in">${insH}</div><div class="io-col io-col-out">${outH}</div></div>`:'';
    const bodyH=renderNodeBody(n);
    el.innerHTML=`<div class="node-accent"></div>
      <div class="node-head"><div class="node-icon"><i data-lucide="${escAttr(n.icon)}"></i></div>
      <div class="node-titles"><div class="node-cat" data-catlabel>${esc(c.label)}</div><div class="node-title" data-titletext>${esc(n.title)}</div></div></div>
      <div class="node-live" data-live></div>
      ${ioH}
      ${bodyH?`<div class="node-body" data-body>${bodyH}</div>`:''}`;
    // idle live-value placeholder via textContent (never innerHTML) so a unit
    // is inert markup-wise regardless of where it came from.
    if(n.live)el.querySelector('[data-live]').textContent=n.unit?('— '+n.unit):'—';
  }
  world.appendChild(el);nodes[n.id]={def:n,el};
  markRequiredPorts(n.id);
  if(n.faceplate&&n.shelly)loadShellyOverview(n.id,(n.shelly||{}).id);
  if(window.lucide)lucide.createIcons();return el;
}

// loadShellyOverview fills the faceplate from the device (one HTTP-RPC round
// trip): the real per-channel NAMES (Switch.GetConfig.name) onto each
// display, and the schedule clock icon on any channel with an on-board
// weekly schedule. Best-effort: an unreachable device just leaves the CH-N
// placeholders and no clocks (config runs over HTTP-RPC, independent of the
// MQTT link).
// RGBW2 COLOR-mode effect vocabulary, VERIFIED from the official Gen1
// doc (#shelly-rgbw2-color-settings-color-0): 0-4, NOT the Bulb's 0-6.
// id 4 is "Red/green change" on the RGBW2 (it is "Breath" on the Bulb -
// ids are NOT portable across devices). [id, name]; 0 = off/static.
const RGBW2_EFFECTS=[['0','Off (static)'],['1','Meteor shower'],['2','Gradual change'],['3','Flash'],['4','Red/green change']];
async function loadShellyOverview(nodeId,storeId){
  if(!storeId)return;
  const nd0=nodes[nodeId];
  // seed the light faceplate controls (effect + colour + state) from the
  // device so a running effect is VISIBLE and a manual change never starts
  // from a stale 0 - the same seed the cockpit card does.
  if(nd0&&nd0.def.shelly&&(nd0.def.shelly.channels||[]).some(c=>c.kind==='color'||c.kind==='white')){
    seedShellyLights(nodeId,storeId);
  }
  try{
    const r=await fetch('shelly/'+storeId+'/overview',{credentials:'same-origin'});
    if(!r.ok)return;
    const d=await r.json(),nd=nodes[nodeId];if(!nd)return;
    for(const k in (d.names||{})){const nm=nd.el.querySelector(`.sh-row[data-ch="${Number(k)}"] [data-chname]`);if(nm&&d.names[k])nm.textContent=d.names[k];}
    if(d.gen1)applyShelly1Schedule(nd,d.schedules||{});
    else applyShellySchedule(nd,d.jobs||[]);
  }catch(_){/* device unreachable: placeholders stay, no error */}
}
// seedShellyLights fills each light row's effect selector + sliders +
// on/off state from the device's current values (per light channel), so
// the faceplate shows the ACTIVE effect at a glance.
async function seedShellyLights(nodeId,storeId){
  const nd=nodes[nodeId];if(!nd)return;
  for(const ch of (nd.def.shelly.channels||[])){
    if(ch.kind!=='color'&&ch.kind!=='white')continue;
    try{
      const r=await fetch('shelly/'+storeId+'/gen1/channel/'+ch.id,{credentials:'same-origin'});
      if(!r.ok)continue;
      const d=await r.json(),cur=nodes[nodeId];if(!cur||!d||!d.state)continue;
      paintLightSeed(cur.el.querySelector(`.sh-lrow[data-ch="${ch.id}"]`),d.state);
    }catch(_){/* unreachable: placeholders stay */}
  }
}
// paintLightSeed applies a light channel's current device state to its
// faceplate row: state on/off, the sliders, the effect selector + swatch.
function paintLightSeed(row,st){
  if(!row||!st)return;
  const on=st.ison===true; row.classList.toggle('on',on);
  const se=row.querySelector('[data-chstate]'); if(se) se.textContent=on?'on':'off';
  row.querySelectorAll('[data-lslider]').forEach(sl=>{
    const map={r:'red',g:'green',b:'blue'}, k=map[sl.getAttribute('data-lslider')]||sl.getAttribute('data-lslider');
    const v=Number(st[k]); if(isFinite(v)) sl.value=v;
  });
  const eff=row.querySelector('[data-leff]'); if(eff){ const e=Number(st.effect); if(isFinite(e)) eff.value=String(e); }
  paintLightSwatch(row);
}
// paintLightSwatch tints the row's colour swatch from its RGB sliders.
function paintLightSwatch(row){
  const sw=row&&row.querySelector('[data-lswatch]'); if(!sw)return;
  const g=k=>{const s=row.querySelector(`[data-lslider="${k}"]`);return s?Math.max(0,Math.min(255,Number(s.value)||0)):0;};
  sw.style.background='rgb('+g('r')+','+g('g')+','+g('b')+')';
  sw.style.backgroundImage='none';
}
// applyShelly1Schedule is the Gen1 sibling: schedules come as per-channel
// rule-string sets ("0700-0123456-on"; weekday digits 0=Monday) instead
// of cron jobs. The clock lights when a channel's schedule is enabled and
// has rules; "next" is computed from the rule strings.
function applyShelly1Schedule(nd,schedules){
  nd.el.querySelectorAll('.sh-row').forEach(row=>{
    const ch=String(Number(row.dataset.ch)),s=schedules[ch];
    const active=!!(s&&s.enabled&&(s.rules||[]).length);
    const clk=row.querySelector('[data-chclock]');if(clk)clk.hidden=!active;
    const nx=row.querySelector('[data-chnext]');
    if(nx)nx.textContent=active?(t=>t?('next '+t.hhmm):'')(shelly1RulesNext(s.rules)):'';
  });
}
// shelly1RulesNext picks the next firing rule of a Gen1 rule set. Fixed
// rules are exact; sun rules order approximately (sunrise 06:00 / sunset
// 18:00 — the device computes the real time) and label as the token.
function shelly1RulesNext(rules){
  const now=new Date();let best=null;
  for(const rule of (rules||[])){
    // fixed "HHMM-DAYS-on|off" or sun "HHMM(asr|bsr|ass|bss)-DAYS-on|off"
    // (HHMM = zero-padded offset magnitude; a=after, b=before).
    const m=/^(\d{2})(\d{2})(asr|bsr|ass|bss)?-([0-6]{1,7})-(on|off)$/.exec(String(rule||''));
    if(!m)continue;
    // Gen1 weekday digits are 0=Monday..6=Sunday; JS getDay() is 0=Sunday.
    const days=m[4].split('').map(d=>(Number(d)+1)%7);
    let mins,label;
    if(m[3]){
      const ev=m[3].slice(1)==='sr'?'sunrise':'sunset';
      const sign=m[3].charAt(0)==='b'?-1:1;
      const off=sign*(Number(m[1])*60+Number(m[2]));
      mins=(ev==='sunrise'?360:1080)+off; // approx ordering; device computes the real time
      label=ev+(off?((off>0?'+':'')+off+'m'):'');
    }else{mins=Number(m[1])*60+Number(m[2]);label=m[1]+':'+m[2];}
    for(let add=0;add<8;add++){const day=(now.getDay()+add)%7;if(!days.includes(day))continue;
      const at=add*1440+mins-(now.getHours()*60+now.getMinutes());
      if(at>=0){if(!best||at<best.at)best={at,hhmm:label+' '+m[5]};break;}}
  }
  return best;
}
// applyShellySchedule lights the clock icon and shows a concise "next
// scheduled action" (time + on/off) per channel from the device's schedule
// jobs. A channel has a schedule when a job's calls target its switch id.
function applyShellySchedule(nd,jobs){
  const byCh={};
  for(const j of jobs){const t=shellyCronNext(j.timespec);for(const c of (j.calls||[])){const cid=c.params&&c.params.id;if(cid==null||!t)continue;
    const on=!!(c.params&&c.params.on),ch=Number(cid);const cur=byCh[ch];if(!cur||t.at<cur.at)byCh[ch]={at:t.at,txt:t.hhmm+' '+(on?'on':'off')};}}
  nd.el.querySelectorAll('.sh-row').forEach(row=>{
    const ch=Number(row.dataset.ch),hit=byCh[ch];
    const clk=row.querySelector('[data-chclock]');if(clk)clk.hidden=!hit;
    const nx=row.querySelector('[data-chnext]');if(nx)nx.textContent=hit?('next '+hit.txt):'';
  });
}
// shellyCronNext computes the next fire of a weekly timespec as a
// minutes-from-now ordering key plus a display label. Handles both the
// fixed form "sec min hour * * dow" (label HH:MM) and the sun-token form
// "@sunrise[+/-<n>m] dom mon dow" (label "sunrise+30m" - the exact time
// is computed on the device; ordering approximates sunrise 06:00 /
// sunset 18:00 purely to pick which action is "next").
function shellyCronNext(ts){
  const p=String(ts||'').trim().split(/\s+/);
  const sun=/^@(sunrise|sunset)([+-]\d+[smh]?)?$/i.exec(p[0]||'');
  const now=new Date();
  if(sun){
    const dowRaw=p[3]!=null?p[3]:'*';
    const dow=dowRaw==='*'?[0,1,2,3,4,5,6]:dowRaw.split(',').map(x=>parseInt(x,10)).filter(x=>!isNaN(x));if(!dow.length)return null;
    let off=sun[2]||''; if(off&&!/[smh]$/i.test(off)) off+='m';
    const approx=sun[1].toLowerCase()==='sunrise'?360:1080;
    for(let add=0;add<8;add++){const day=(now.getDay()+add)%7;if(!dow.includes(day))continue;
      const mins=add*1440+approx-(now.getHours()*60+now.getMinutes());if(mins>=0)return {at:mins,hhmm:sun[1].toLowerCase()+off};}
    return null;
  }
  if(p.length<6)return null;
  const min=parseInt(p[1],10),hour=parseInt(p[2],10);if(isNaN(min)||isNaN(hour))return null;
  const dow=p[5]==='*'?[0,1,2,3,4,5,6]:p[5].split(',').map(x=>parseInt(x,10)).filter(x=>!isNaN(x));if(!dow.length)return null;
  for(let add=0;add<8;add++){const day=(now.getDay()+add)%7;if(!dow.includes(day))continue;
    const mins=add*1440+hour*60+min-(now.getHours()*60+now.getMinutes());if(mins>=0)return {at:mins,hhmm:String(hour).padStart(2,'0')+':'+String(min).padStart(2,'0')};}
  return null;
}

// shellyFaceplateHTML renders the device-faithful Shelly faceplate: a
// header (identity + total power + online) and one symmetric row per
// channel — a blue bordered display (name · live watts · on/off styling,
// no 1/0 bit), a divider, a clickable relay switch, and a clock slot for a
// set schedule. The graph ports stay minimal at the row edges: a control
// input (left) and the readout outputs (right), each a real .port socket
// so wiring is unchanged. Interactive bits carry data-noselectdrag so a
// click drives the device instead of dragging the node.
// SHELLY_PLABEL / SHELLY_PTIP: the on-block Anschlussbezeichnung + the hover
// tooltip for every faceplate port. Control input on the left; State / Power /
// Input readouts on the right — no cryptic single letters, every port named.
const SHELLY_PLABEL={relay:'Control',state:'State',power:'Power',input:'Input'};
const SHELLY_PTIP={
  relay:'Relay control — a signal here switches the relay on/off',
  state:'Relay on/off state',
  power:'Current power draw in watts',
  input:'State of the physical SW input',
};
function shellyFaceplateHTML(n){
  const sh=n.shelly||{}, model=sh.model||'', gen1=sh.gen===1;
  const metric=(k,u)=>`<div class="sh-m"><span class="sh-mv" data-${k}>—</span><span class="sh-ml">${u}</span></div>`;
  const outPorts=(c)=>n.ports.out.filter(p=>p.ch===c).map(p=>`<div class="sh-orow"><span class="sh-plabel">${esc(SHELLY_PLABEL[p.srole]||p.label||p.id)}</span><div class="port io-out sh-po" data-port="${escAttr(n.id+':'+p.id)}" data-tip="${escAttr(SHELLY_PTIP[p.srole]||p.label||'')}"><span class="socket${kindClass(p.kind)}"></span></div></div>`).join('');
  const inPort=(id,tip,kind)=>`<div class="port io-in sh-pin" data-port="${escAttr(n.id+':'+id)}" data-tip="${escAttr(tip)}"><span class="socket${kindClass(kind)}"></span></div>`;
  // one row per channel; a light-class channel (color/white) renders the
  // colour+gain control, a relay channel the switch (existing shape).
  const relayRow=(c)=>{
    const meterCh=(sh.channels||[]).some(x=>x.id===c&&x.meter);
    const grid=gen1?(meterCh?metric('chw','W'):''):(metric('chw','W')+metric('chv','V')+metric('cha','A')+metric('chhz','Hz'));
    return `<div class="sh-row" data-ch="${c}">
      <div class="sh-inport">${inPort('sw'+c+'_set',SHELLY_PTIP.relay,'bool')}<span class="sh-plabel">${esc(SHELLY_PLABEL.relay)}</span></div>
      <div class="sh-disp" data-chsettings="${c}" data-noselectdrag title="Channel settings">
        <div class="sh-disp-top"><span class="sh-disp-name" data-chname>CH${c+1}</span><span class="sh-in-led" data-chin title="Physical input state"></span><span class="sh-clock" data-chclock hidden><i data-lucide="clock"></i></span></div>
        ${grid?`<div class="sh-grid">${grid}</div>`:''}
        <div class="sh-disp-foot"><span class="sh-disp-state" data-chstate>off</span><span class="sh-next" data-chnext></span></div>
      </div>
      <div class="sh-div"></div>
      <button type="button" class="sh-sw" data-chsw="${c}" data-noselectdrag title="Toggle relay"><span class="sh-track"><span class="sh-thumb"></span></span></button>
      <div class="sh-pout">${outPorts(c)}</div>
    </div>`;
  };
  const lightRow=(ch)=>{
    const c=ch.id, color=ch.kind!=='white';
    const rgb=color?['r','g','b'].map(k=>`<label class="sh-lc"><span>${k.toUpperCase()}</span><input type="range" min="0" max="255" value="0" data-lslider="${k}" data-noselectdrag></label>`).join(''):'';
    return `<div class="sh-row sh-lrow" data-ch="${c}">
      <div class="sh-inport">${inPort('li'+c+'_set',SHELLY_PTIP.relay,'bool')}<span class="sh-plabel">On/off</span>
        <div class="sh-inport" style="margin-top:6px;">${inPort('li'+c+'_gain','Brightness/gain 0-100','float')}<span class="sh-plabel">${color?'Gain':'Bright'}</span></div>
      </div>
      <div class="sh-disp" data-chsettings="${c}" data-noselectdrag title="Light settings">
        <div class="sh-disp-top"><span class="sh-disp-name" data-chname>${color?'Color':'White'} ${c+1}</span><span class="sh-swatch" data-lswatch></span><span class="sh-clock" data-chclock hidden><i data-lucide="clock"></i></span></div>
        <div class="sh-grid">${metric('chw','W')}</div>
        <div class="sh-lctl" data-noselectdrag>
          <label class="sh-lc"><span>${color?'Gain':'Bright'}</span><input type="range" min="0" max="100" value="0" data-lslider="${color?'gain':'brightness'}" data-noselectdrag></label>
          ${rgb}
          ${color?`<label class="sh-lc"><span>Effect</span><select class="sh-leff" data-leff data-noselectdrag>${RGBW2_EFFECTS.map(e=>`<option value="${e[0]}">${esc(e[1])}</option>`).join('')}</select></label>`:''}
        </div>
        <div class="sh-disp-foot"><span class="sh-disp-state" data-chstate>off</span><span class="sh-next" data-chnext></span></div>
      </div>
      <div class="sh-div"></div>
      <button type="button" class="sh-sw" data-lsw="${c}" data-noselectdrag title="Toggle light"><span class="sh-track"><span class="sh-thumb"></span></span></button>
      <div class="sh-pout">${outPorts(c)}</div>
    </div>`;
  };
  const rows=(sh.channels||[]).map(ch=>(ch.kind==='color'||ch.kind==='white')?lightRow(ch):relayRow(ch.id)).join('');
  return `<div class="sh-head">
      <div class="sh-badge"><i data-lucide="cpu"></i></div>
      <div class="sh-id"><div class="sh-name" data-titletext>${esc(n.title)}</div><div class="sh-model">${esc(model)}</div></div>
      <div class="sh-meta"><div class="sh-total"><span data-shtotal>—</span><span class="sh-u">W</span></div><div class="sh-online" data-shonline title="Device status"></div></div>
    </div>
    <div class="sh-rows">${rows}</div>`;
}

// markRequiredPorts flags every required (non-optional, kind-known) input
// that is currently unwired, at the exact port, so the editor points at
// the missing connection before a run is ever attempted (a run also
// rejects it server-side). Idempotent; called on build and after every
// wiring change. `nodeId` limits the sweep to one node; omit for all.
export function markRequiredPorts(nodeId){
  const ids=nodeId?[nodeId]:Object.keys(nodes);
  for(const id of ids){const nd=nodes[id];if(!nd)continue;
    for(const p of nd.def.ports.in){
      const pe=nd.el.querySelector(`[data-port="${cssAttr(id+':'+p.id)}"]`);if(!pe)continue;
      const required=p.optional===false&&!!p.kind,wired=wires.some(o=>o.to===id+':'+p.id);
      pe.classList.toggle('io-req',required&&!wired);
      // A Shelly relay or a generic device control is single-use: once driven,
      // it renders "in use" (and the wiring layer refuses a second driver). On
      // a faceplate, the clickable switch for that channel goes inert.
      pe.classList.toggle('io-driven',(p.srole==='relay'||p.srole==='control')&&wired);
      if(p.srole==='relay'){
        // graph-driven: the manual switch (relay data-chsw, or light
        // data-lsw) goes inert. A light row also locks its colour/gain
        // sliders when the on/off control is graph-driven.
        const sw=nd.el.querySelector(`[data-chsw="${p.ch}"]`)||nd.el.querySelector(`[data-lsw="${p.ch}"]`);
        if(sw)sw.classList.toggle('sh-locked',wired);
        if(p.light){const row=nd.el.querySelector(`.sh-lrow[data-ch="${p.ch}"]`);if(row)row.classList.toggle('sh-ldriven',wired);}
      }
      // a graph-driven gain input dims that row's manual gain/colour too.
      if(p.srole==='gain'){const row=nd.el.querySelector(`.sh-lrow[data-ch="${p.ch}"]`);if(row)row.classList.toggle('sh-gaindriven',wired);}
      // control_loop: a wired target/enable owns the value, so the block's own
      // field/switch goes inert — the same rule as a graph-driven Shelly relay.
      // cl-wired on the row swaps the widget for the live value, so the row
      // shows what the graph feeds rather than a stale manual number.
      // Only the loop emits data-clctl, so this is inert for every other block.
      const w=nd.el.querySelector(`[data-clctl="${cssAttr(p.id)}"]`);
      if(w){
        w.classList.toggle('cl-locked',wired);
        // The rocker is a container, so reach its buttons: the CSS lock stops
        // the mouse, disabled also stops the keyboard.
        if(w.disabled!==undefined)w.disabled=wired;
        w.querySelectorAll('button').forEach(b=>{b.disabled=wired;});
        const row=w.closest('.ro-row'); if(row)row.classList.toggle('cl-wired',wired);
      }
    }
  }
}
// cssAttr escapes a value for use inside an attribute selector's quotes.
function cssAttr(v){return String(v).replace(/["\\]/g,'\\$&');}
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
    const kindp={k:'Value type',v:'float',kind:'mqtt-kind',base,
      opts:[{v:'bool',l:'Bool (on/off)'},{v:'float',l:'Float (number)'},{v:'text',l:'Text'}]};
    let props;
    if(isSrc){
      // Optional JSON field: pull one value out of a JSON payload (e.g. a
      // Shelly status snapshot - output, apower, temperature.tC). Empty =
      // the whole payload is the value. run.js folds it into the channel
      // address as "<topic>#<path>"; the mqtt: driver splits it back.
      props=[topic,kindp,{k:'Field (JSON)',v:'',kind:'mqtt-path',mqtt:'path',inspectorOnly:true}];
    }else{
      // Sink mode: plain publish of the value, or a Shelly Gen2 Switch.Set
      // RPC that switches a relay. Switch.Set is bool-only (the driver
      // rejects other kinds), so picking it retypes the block to bool.
      // Mode+relay fold into the address as "<topic>#Switch.Set:<n>".
      props=[topic,kindp,
        {k:'Mode',v:'publish',kind:'enum',mqtt:'method',inspectorOnly:true,
          opts:[{v:'publish',l:'Publish value'},{v:'Switch.Set',l:'Shelly Switch.Set (bool)'}]},
        {k:'Relay',v:'0',kind:'number',mqtt:'relay',inspectorOnly:true},
        {k:'Retain',v:'false',param:'retain',kind:'enum',opts:[{v:'false',l:'Off'},{v:'true',l:'On'}]}];
    }
    // Seed kind is float (the catalog's); the value-type selector re-types
    // the node AND updates this port kind (inspector), so type-checked
    // wiring follows a Bool/Text choice - e.g. Switch.Set needs a bool in.
    return {cat:'mqtt',icon:NAME_ICON[name]||'radio',title:name,type:t,implemented:true,live:isSrc,props,
      ports:isSrc?{in:[],out:[{id:'out',label:'Out',kind:'float'}]}:{in:[{id:'in',label:'In',kind:'float',optional:false}],out:[]}};
  }
  // Telegram blocks. MUST come before the generic source.channel /
  // sink.channel branches below - the four blocks reuse those engine
  // types and would otherwise render as GPIO/system cards. The Chat
  // prop holds the raw chat id (picked from the allowlist); run.js
  // serializes the channel param as telegram:<mode>:<id>#<node-id>
  // (the #slot keeps two blocks on the same chat bindable). The
  // command word serializes as telegram:cmd:<wort>#<node-id>.
  if(NAME_CAT[name]==='telegram'){
    const isSrc=t.indexOf('source')===0;
    const chat={k:'Chat',v:'',param:'channel',kind:'tg-chat',mode:isSrc?'chat':'send'};
    let props,live=false;
    if(t==='sink.channel'){ // Telegram Send: edge -> fixed message
      props=[chat,{k:'Message',v:'Doorbell rang!',param:'message'}];
    }else if(t==='source.channel'){ // Telegram Command: word -> bool pulse
      props=[{k:'Command',v:'light on',param:'channel',kind:'tg-cmd'}];
    }else if(t==='source.channel.text'){ // Telegram Receive: raw text
      props=[chat];live=true;
    }else{ // sink.channel.text - Telegram Send text: text input -> message
      props=[chat];
    }
    const k=kindOfType(t);
    return {cat:'telegram',icon:NAME_ICON[name]||'send',title:name,type:t,implemented:true,live,props,
      ports:isSrc?{in:[],out:[{id:'out',label:'Out',kind:k}]}:{in:[{id:'in',label:'In',kind:k,optional:false}],out:[]}};
  }
  // NFC reader blocks. Also before the generic channel branches: they
  // reuse source.channel(.text) and would otherwise render as a GPIO
  // card (line picker + pin options) resp. a system card. Both channels
  // are baked by the catalog - one block pair per detected reader, like
  // the sys metrics, no picker; the UID text source shows the last read
  // tag live on the card.
  if(NAME_CAT[name]==='nfc'){
    const isUID=t==='source.channel.text';
    return {cat:'nfc',icon:NAME_ICON[name]||'nfc',title:name,type:t,implemented:true,live:isUID,
      props:[{k:'Channel',v:NAME_CHANNEL[name]||'',param:'channel',inspectorOnly:true}],
      ports:{in:[],out:[{id:'out',label:isUID?'UID':'Tag',kind:isUID?'text':'bool'}]}};
  }
  // Shelly device module: one finished, capability-aware faceplate per
  // adopted device. It is a COMPOSITE - not an engine node - that expands
  // into per-channel mqtt: source/sink nodes at run time (run.js). Ports
  // are built from the device's channels: a relay control INPUT (drive
  // on/off, single-use) + a state OUTPUT and, on metered channels, a power
  // OUTPUT per channel (readouts, freely consumable). The mac/prefix travel
  // on def.shelly so the run can compose the topic bindings.
  // Generic readout/sensor module: one capability-driven faceplate per
  // adopted readout device (Protect sensors first). It is a COMPOSITE - not
  // an engine node - that expands into per-readout source nodes at run time
  // (run.js). Ports are OUTPUT-only (readouts, freely consumable, no
  // exclusivity); the payload's channel refs travel on def.readout so the
  // run binds them by prefix. Keyed off the readout payload, so any device
  // class flows through the same path.
  if(t==='midea.control_loop')return climateLoopDef(name);
  if(NAME_READOUT[name])return readoutDef(name);
  if(NAME_CAT[name]==='shelly')return shellyDef(name);
  if(t==='source.channel'||t==='sink.channel'){const isSrc=t==='source.channel',gc=NAME_CAT[name]||'gpio';
    // Like the other blocks (staircase shows Mode/Hold, lamp shows
    // Output/Channel), the GPIO card shows its pin options; they stay
    // editable in the inspector. Defaults match the driver's prior fixed
    // behaviour (input: pull-up + active-low; output: active-high, initial
    // low) so an untouched block does not regress.
    const line={k:'Line',v:'',param:'channel',kind:'gpio-line'};
    const props=isSrc?[line,
      {k:'Bias',param:'bias',kind:'enum',v:'pullup',opts:[{v:'pullup',l:'Pull-up'},{v:'pulldown',l:'Pull-down'},{v:'none',l:'None'}]},
      {k:'Level',param:'active_level',kind:'enum',v:'low',opts:[{v:'low',l:'Active-Low'},{v:'high',l:'Active-High'}]},
      {k:'Debounce',param:'debounce_ms',kind:'number',v:'0',suffix:'ms'}]
    :[line,
      {k:'Initial',param:'initial',kind:'enum',v:'low',opts:[{v:'low',l:'Low'},{v:'high',l:'High'}]},
      {k:'Level',param:'active_level',kind:'enum',v:'high',opts:[{v:'high',l:'Active-High'},{v:'low',l:'Active-Low'}]}];
    return {cat:gc,icon:NAME_ICON[name]||(CAT[gc]&&CAT[gc].icon)||'cpu',title:name,type:t,implemented:true,props,
      ports:isSrc?{in:[],out:[{id:'out',label:'Out',kind:'bool'}]}:{in:[{id:'in',label:'In',kind:'bool',optional:false}],out:[]}};}
  if(t==='source.channel.float'||t==='source.channel.text'||t==='sink.channel.float'||t==='sink.channel.text'){
    // Float/Text channel blocks (e.g. system telemetry): the channel is
    // fixed by the catalog (no picker), kept as an inspector-only param so
    // it serializes; the card shows the live value (with its unit) in a run.
    const isSrc=t.indexOf('source')===0,gc=NAME_CAT[name]||'system',unit=NAME_UNIT[name]||'',k=kindOfType(t);
    return {cat:gc,icon:NAME_ICON[name]||(CAT[gc]&&CAT[gc].icon)||'gauge',title:name,type:t,implemented:true,unit,live:true,
      props:[{k:'Channel',v:NAME_CHANNEL[name]||'',param:'channel',inspectorOnly:true}],
      ports:isSrc?{in:[],out:[{id:'out',label:'Out',kind:k}]}:{in:[{id:'in',label:'In',kind:k,optional:false}],out:[]}};}
  // Constant (input.constant.*): a fixed, held value source with a
  // value-type selector (bool/float/text) that re-types the node and a
  // value editor. Seeded as the float variant; the selector retypes it.
  if(t&&t.indexOf('input.constant.')===0)return constantDef(name,t);
  // The base engine-backed blocks: Push-button (input.manual, momentary
  // press), Switch (input.toggle, held on/off), OR (logic.or), Staircase
  // (time.staircase) and Lamp (output.lamp). Ports and kinds come from
  // the catalog (the single source of truth); the control is the only
  // per-block choice. Setting `type` here is what lets a dropped base
  // block run again (serializeGraph keeps only typed, implemented nodes).
  if(t==='input.manual'||t==='input.toggle'||t==='logic.or'||t==='output.lamp'||t==='time.staircase'){
    const base={cat:NAME_CAT[name]||'input',icon:NAME_ICON[name]||CAT[NAME_CAT[name]||'input'].icon,title:name,type:t,implemented:true,props:[],ports:portsFromCatalog(name)};
    if(t==='input.manual')return {...base,control:'press'};          // momentary
    if(t==='input.toggle')return {...base,control:'switch',on:false,live:true}; // held
    if(t==='time.staircase')return {...base,control:'slider',value:180,min:1,max:600,step:1,unit:'s',vlabel:'Hold time'};
    return base; // logic.or, output.lamp: no on-card control
  }
  // Catalog-only (decorative) blocks: no engine node yet, so no `type` -
  // they render with invented generic ports and stay inert until their
  // engine node lands (they are skipped by serializeGraph).
  const c=cat||NAME_CAT[name]||'logic',icon=NAME_ICON[name]||CAT[c].icon,base={cat:c,icon,title:name};
  if(c==='input')return{...base,props:[{k:'Input',v:'I?',accent:true}],ports:{in:[],out:['Q']},control:'switch',on:false};
  if(c==='logic')return{...base,props:[],ports:{in:['A','B'],out:['Q']}};
  if(c==='time')return{...base,props:[{k:'Mode',v:'—'}],ports:{in:['Tr'],out:['Q']},control:'slider',value:3,min:.5,max:30,step:.5,unit:'s',vlabel:'Time'};
  if(c==='memory')return{...base,props:[{k:'Value',v:'0'}],ports:{in:['S','R'],out:['Q']},control:'switch',on:false};
  if(c==='output')return{...base,props:[{k:'Output',v:'Q?',accent:true}],ports:{in:['AI'],out:[]},control:'switch',on:false};
  return{...base,props:[],ports:{in:['A'],out:['Q']}};}
// constantDef builds the Constant block def for its seed kind. The
// value-type selector (const-kind) re-types it and swaps the value
// editor; the value (const-val) serializes as the engine `value` param,
// typed per the current kind (run.js). Output port kind follows the kind.
// shellyDef builds a Shelly device module from its catalog payload: one
// relay control input + a state (and, if metered, a power) readout output
// per channel. srole tags each port for run-time expansion and for the
// relay single-use rule. live:false — the per-port badges show the live
// state/power (the faceplate visuals are a later step), so no single big
// card readout.
function shellyDef(name){
  const s=NAME_SHELLY[name]; if(!s) return null;
  const chans=(s.channels||[]),inp=[],outp=[];
  for(const ch of chans){
    const n=ch.id;
    if(ch.kind==='color'||ch.kind==='white'){
      // Light-class channel (RGBW2): an on/off control + an optional
      // gain/brightness driver in; state + per-light power out. The
      // control reuses srole:'relay' so the single-driver exclusivity +
      // manual-inert rules apply unchanged.
      inp.push({id:'li'+n+'_set',label:'On/off',kind:'bool',srole:'relay',ch:n,light:true});
      inp.push({id:'li'+n+'_gain',label:ch.kind==='white'?'Brightness':'Gain',kind:'float',srole:'gain',ch:n,light:true,optional:true});
      outp.push({id:'li'+n+'_out',label:'State',kind:'bool',srole:'state',ch:n,light:true});
      outp.push({id:'li'+n+'_pwr',label:'Power',kind:'float',srole:'power',ch:n,light:true});
      continue;
    }
    const lab='CH'+(n+1);
    inp.push({id:'sw'+n+'_set',label:lab,kind:'bool',srole:'relay',ch:n});
    outp.push({id:'sw'+n+'_out',label:'State',kind:'bool',srole:'state',ch:n});
    if(ch.meter)outp.push({id:'sw'+n+'_pwr',label:'Power',kind:'float',srole:'power',ch:n});
    outp.push({id:'in'+n,label:'Input',kind:'bool',srole:'input',ch:n});
  }
  return {cat:'shelly',icon:NAME_ICON[name]||'toggle-right',title:name,type:'shelly.device',implemented:true,live:false,props:[],faceplate:true,
    // gen rides along so the run expansion emits the right topic/payload
    // grammar (0/absent = Gen2, the pre-Gen1 catalogs).
    shelly:{id:s.id,mac:s.mac,prefix:s.prefix,model:s.model,name:s.name,gen:s.gen||0,channels:chans},
    ports:{in:inp,out:outp}};
}
// readoutDef builds a generic readout/sensor module from its catalog
// payload: one OUTPUT port per readout (freely consumable - no control
// input, so none of the relay single-use logic applies) + a live-value
// faceplate. Vendor-neutral: the payload carries fully-formed channel refs,
// so the same builder serves any readout family. cat is the device class
// (drives the palette group); srole:'readout' tags the ports for the run
// expansion + live paint.
function readoutDef(name){
  const s=NAME_READOUT[name]; if(!s) return null;
  const outp=(s.readouts||[]).map(r=>({id:r.key,label:r.label||r.key,kind:r.kind||'float',unit:r.unit||'',srole:'readout'}));
  // Control capabilities (empty for a read-only sensor) become single-driver
  // INPUT ports - srole:'control' reuses the relay exclusivity + faceplate
  // lock. This is what makes the readout module a generic DEVICE module.
  const inp=(s.controls||[]).map(c=>({id:c.key,label:c.label||c.key,kind:c.kind||'float',unit:c.unit||'',srole:'control',options:c.options||[]}));
  return {cat:s.class||'sensor',icon:NAME_ICON[name]||s.icon||'thermometer',title:name,type:'readout.device',implemented:true,live:false,props:[],faceplate:true,
    readout:{id:s.id,class:s.class,name:s.name,model:s.model,readouts:s.readouts||[],controls:s.controls||[]},
    ports:{in:inp,out:outp}};
}
// readoutFaceplateHTML renders the generic readout faceplate: a header
// (name/model/online) and one tile per readout showing its live value +
// unit, with the output socket beside it. It reuses the Shelly faceplate's
// base classes (sh-head/sh-rows/…) for a consistent look, plus ro- hooks
// the run's paintReadout writes into. Read-only: no switches, no controls.
function readoutFaceplateHTML(n){
  const ro=n.readout||{}, model=ro.model||'';
  // Control INPUT rows first (a driver wires in here); then readout OUTPUT
  // rows. An unwired control shows "—"; the run's paintReadout writes the live
  // driven value into data-roctlval when the port is bound.
  const ctlrows=(n.ports.in||[]).map(p=>{
    const unit=p.unit?`<span class="ro-unit">${esc(p.unit)}</span>`:'';
    return `<div class="ro-row ro-ctl" data-roctl="${escAttr(p.id)}">
      <div class="port io-in ro-pi" data-port="${escAttr(n.id+':'+p.id)}" data-tip="${escAttr((p.label||p.id)+' control')}"><span class="socket${kindClass(p.kind)}"></span></div>
      <div class="ro-disp"><span class="ro-label">${esc(p.label||p.id)}</span><span class="ro-metric"><span class="ro-val" data-roctlval>—</span>${unit}</span></div>
    </div>`;
  }).join('');
  const rows=(n.ports.out||[]).map(p=>{
    const unit=p.unit?`<span class="ro-unit">${esc(p.unit)}</span>`:'';
    return `<div class="ro-row" data-ro="${escAttr(p.id)}">
      <div class="ro-disp"><span class="ro-label">${esc(p.label||p.id)}</span><span class="ro-metric"><span class="ro-val" data-roval>—</span>${unit}</span></div>
      <div class="port io-out ro-po" data-port="${escAttr(n.id+':'+p.id)}" data-tip="${escAttr((p.label||p.id)+' readout')}"><span class="socket${kindClass(p.kind)}"></span></div>
    </div>`;
  }).join('');
  return `<div class="sh-head ro-head">
      <div class="sh-badge"><i data-lucide="${escAttr(n.icon||'thermometer')}"></i></div>
      <div class="sh-id"><div class="sh-name" data-titletext>${esc(n.title)}</div><div class="sh-model">${esc(model)}</div></div>
      <div class="sh-meta"><div class="sh-online ro-online" data-roonline title="Device status"></div></div>
    </div>
    <div class="sh-rows ro-rows">${ctlrows}${rows}</div>`;
}
// climateLoopDef builds the Midea control_loop block: a registered stateful
// engine node (NOT JS-expanded - it reaches Build with its "device" param and
// its wired input edges, and drives the device through the monitor seam, not a
// sink channel). Wired INPUT ports are the control variables (external room
// temperature is the key one); OUTPUT ports are the live readouts; the bound
// device id (baked into NAME_CHANNEL) rides in the "device" param, profile +
// target VPD are inspector params. faceplate + readout:{climate} routes the live
// output values through paintReadout onto the faceplate rows.
// CLIMATE_PTIP: the hover sentence for every control_loop port. The labels say
// WHAT a port is, these say what to wire into it — the room sensor vs the target
// is the distinction users get wrong, so both tips name it explicitly.
const CLIMATE_PTIP={
  room_temp:'Measured room temperature — wire your external room sensor here. This is the value the loop controls. The device\'s own built-in sensor is read automatically and needs no wire.',
  room_hum:'Measured room humidity in % — optional. Needed for the dew point and VPD readouts.',
  target:'The temperature you WANT, in °C — not a sensor. Leave unwired and dial it in with the up/down rocker on this block (17–30 °C, half-degree steps); wire it only if the graph should own it (then the wire wins).',
  enable:'Runs the loop. Leave unwired and use the ON/OFF switch on this block; wire it only if the graph should own it (then the wire wins). Off does not just stop controlling — it switches the device OFF.',
  light_on:'Grow light on/off, fed forward into the decision. Only used by the Cultivation (VPD) profile.',
  light_in_s:'Seconds until the grow light switches, fed forward into the decision. Only used by the Cultivation (VPD) profile.',
};
// The on-block thermostat rocker's range + step: the device's own setpoint
// window, which the node clamps to server-side as well (mideaengine:
// minTarget/maxTarget) — this is only the convenience half of the guard.
const CLIMATE_TARGET_MIN=17, CLIMATE_TARGET_MAX=30, CLIMATE_TARGET_STEP=0.5;
// climateClampTarget snaps a target into the device's range on the step grid.
function climateClampTarget(v){
  const n=Math.round(Number(v)/CLIMATE_TARGET_STEP)*CLIMATE_TARGET_STEP;
  if(!Number.isFinite(n))return 25;
  return Math.min(CLIMATE_TARGET_MAX,Math.max(CLIMATE_TARGET_MIN,n));
}
// climateFmtTarget shows the half-degree without trailing noise: 21.5 / 22.
export function climateFmtTarget(v){
  const n=Number(v);
  return Number.isInteger(n)?String(n):n.toFixed(1);
}
// climateStepTarget moves the target one rocker press up (+1) or down (-1),
// stopping at the ends of the device's range. Returns the value unchanged when
// there is nowhere left to go, so the caller can skip a pointless send.
export function climateStepTarget(cur,dir){
  return climateClampTarget(Number(cur)+dir*CLIMATE_TARGET_STEP);
}
function climateLoopDef(name){
  const dev=NAME_CHANNEL[name]||'';
  // optional:false on room_temp is load-bearing: markRequiredPorts tests
  // `p.optional===false`, so an absent key would leave the one mandatory input
  // unmarked. srole:'control' on target/enable buys the single-driver
  // exclusivity (io-driven + a refused second wire) the Shelly relay uses.
  const inp=[
    {id:'room_temp',label:'Room temperature (external sensor)',kind:'float',optional:false},
    {id:'room_hum',label:'Humidity',kind:'float',optional:true},
    {id:'target',label:'Target temperature',kind:'float',optional:true,srole:'control'},
    {id:'enable',label:'Enable',kind:'bool',optional:true,srole:'control'},
    {id:'light_on',label:'Grow light on (feedforward)',kind:'bool',optional:true},
    {id:'light_in_s',label:'Seconds to light change (feedforward)',kind:'float',optional:true},
  ];
  const outp=[
    {id:'status',label:'Control stage',kind:'text'},
    {id:'deviation',label:'Deviation from target',kind:'float',unit:'°C'},
    {id:'tendency',label:'Trend',kind:'float',unit:'°C/min'},
    {id:'dewpoint',label:'Dew point',kind:'float',unit:'°C'},
    {id:'vpd',label:'VPD',kind:'float',unit:'kPa'},
    {id:'cool_rate',label:'Cooling rate',kind:'float',unit:'°C/min'},
    {id:'alarm',label:'Alarm',kind:'text'},
  ];
  // cardOnly props back the two on-block widgets: they live on the faceplate,
  // not in the inspector (an inspector copy would desync the widget and skip
  // the live send). pkind makes serializeGraph emit a real float/bool — a
  // string would fail the engine's coerceValue and silently fall back to the
  // param default.
  return {cat:'climate-loop',icon:NAME_ICON[name]||'gauge',title:name,type:'midea.control_loop',implemented:true,live:false,faceplate:true,
    readout:{climate:true},
    help:NAME_DESC[name]||'',
    props:[
      {k:'Device',v:dev,param:'device',inspectorOnly:true},
      {k:'Profile',v:'komfort',param:'profile',kind:'enum',opts:[{v:'komfort',l:'Comfort'},{v:'kultivierung',l:'Cultivation (VPD)'},{v:'buero',l:'Office'},{v:'heizen',l:'Heating'}]},
      {k:'Target VPD (kPa)',v:'0',param:'target_hum',kind:'number',pkind:'float'},
      {k:'Target temperature',v:25,param:'target_temp',pkind:'float',cardOnly:true},
      {k:'Enable',v:true,param:'enabled',pkind:'bool',cardOnly:true},
    ],
    ports:{in:inp,out:outp}};
}
// climatePropVal reads a param-tagged prop's current value off the def.
function climatePropVal(n,param,dflt){
  const p=(n.props||[]).find(p=>p.param===param);
  return p?p.v:dflt;
}
// upgradeClimateDef re-derives a stored control_loop's ports/props/help from the
// current climateLoopDef. A graph persists the WHOLE def and replays it verbatim
// on load, so without this an already-saved loop block would keep its old
// ambiguous labels forever and — worse — carry no target_temp/enabled prop, so
// the on-block field and switch would render with nothing behind them. The
// user's own values (profile, target VPD, target, enable, the bound device) are
// carried across by param name; only the presentation is refreshed. buildNode is
// the single choke point for both load and drop, so this runs exactly once per
// card build. No-op for every other node type.
function upgradeClimateDef(n){
  if(!n||n.type!=='midea.control_loop')return;
  const fresh=climateLoopDef(n.title);
  const old=n.props||[];
  for(const fp of fresh.props){
    const sp=old.find(p=>p.param===fp.param);
    if(!sp||sp.v==null)continue;
    // An empty stored device must not clobber the id freshly baked from the
    // catalog; a non-empty one wins, so a device that has since left the
    // catalog (switched to standard, removed) keeps its binding.
    if(fp.param==='device'&&sp.v==='')continue;
    fp.v=sp.v;
  }
  // Normalise a stored target into the device's range on the step grid: an
  // earlier build let the target be set outside it, and the node falls back to
  // its default for such a value — the rocker would then show one number while
  // the loop regulated to another.
  const tp=fresh.props.find(p=>p.param==='target_temp');
  if(tp)tp.v=climateClampTarget(tp.v);
  n.ports=fresh.ports;n.props=fresh.props;n.cat=fresh.cat;n.icon=fresh.icon;
  n.readout=fresh.readout;n.faceplate=true;
  n.help=fresh.help||n.help||'';
  // Recolour only a card the user never touched: the loop moved from the cyan
  // Climate category to its own rose one, and an untouched card should follow.
  if(n.color==null||n.color==='#5EC8E5')n.color=(CAT['climate-loop']||{}).color||'#FF6B8B';
}
// climateFaceplateHTML renders the control_loop faceplate: the wired control
// inputs (sockets left) then the live readout rows (paintReadout writes each
// value into data-roval by the output port name). Reuses the readout classes.
function climateFaceplateHTML(n){
  // Each input row carries data-roctl + a data-roctlval slot so a wired port's
  // live value actually paints (paintReadout queries exactly that pair).
  // target/enable additionally carry their own widget, used while the port is
  // unwired; markRequiredPorts dims it the moment a wire lands.
  // A control port carries BOTH its widget and a live-value slot: while the
  // port is unwired the widget is shown and drives the value; once wired, CSS
  // swaps in the slot so the row shows what the GRAPH is actually feeding the
  // loop. Showing a dimmed, stale manual number next to a live wire is exactly
  // the ambiguity this block is being fixed for.
  const live=`<span class="ro-val cl-live" data-roctlval>—</span>`;
  const widget=(p)=>{
    // The target is a thermostat: a set value with an up/down rocker. It only
    // SETS — the measuring is the external sensor's job on the port above.
    if(p.id==='target'){
      const v=climateClampTarget(climatePropVal(n,'target_temp',25));
      // The wired row swaps the whole dial for the live value, so it carries its
      // OWN °C (cl-live): the readout formatter emits a bare number, and a
      // wired target would otherwise read "21.5" with no unit.
      return live+`<span class="ro-unit cl-live">°C</span>
      <span class="cl-dial" data-clctl="target" data-noselectdrag title="Target temperature — the value you want the room to reach">
        <span class="cl-dial-val" data-cldialval>${esc(climateFmtTarget(v))}</span><span class="ro-unit">°C</span>
        <span class="cl-rock">
          <button type="button" class="cl-step" data-clstep="1" title="Warmer (+${CLIMATE_TARGET_STEP} °C)" aria-label="Increase target temperature"><i data-lucide="chevron-up"></i></button>
          <button type="button" class="cl-step" data-clstep="-1" title="Cooler (−${CLIMATE_TARGET_STEP} °C)" aria-label="Decrease target temperature"><i data-lucide="chevron-down"></i></button>
        </span>
      </span>`;
    }
    // The enable control says which state it is IN, in words — a switch whose
    // only cue is its own position is exactly how a running loop came to look
    // like a stopped one.
    if(p.id==='enable'){
      const on=climatePropVal(n,'enabled',true)===true;
      return live+`<span class="cl-en">
        <span class="cl-en-state${on?' on':''}" data-clenstate>${on?'ON':'OFF'}</span>
        <button type="button" class="sh-sw cl-sw${on?' on':''}" data-clctl="enable" data-noselectdrag
          title="Switch the control loop on/off"><span class="sh-track"><span class="sh-thumb"></span></span></button>
      </span>`;
    }
    return live;
  };
  const inrows=(n.ports.in||[]).map(p=>`<div class="ro-row ro-ctl" data-roctl="${escAttr(p.id)}">
      <div class="port io-in ro-pi" data-port="${escAttr(n.id+':'+p.id)}" data-tip="${escAttr(CLIMATE_PTIP[p.id]||((p.label||p.id)+' input'))}"><span class="socket${kindClass(p.kind)}"></span></div>
      <div class="ro-disp"><span class="ro-label">${esc(p.label||p.id)}</span><span class="ro-metric cl-w">${widget(p)}</span></div>
    </div>`).join('');
  const outrows=(n.ports.out||[]).map(p=>{
    const unit=p.unit?`<span class="ro-unit">${esc(p.unit)}</span>`:'';
    return `<div class="ro-row" data-ro="${escAttr(p.id)}">
      <div class="ro-disp"><span class="ro-label">${esc(p.label||p.id)}</span><span class="ro-metric"><span class="ro-val" data-roval>—</span>${unit}</span></div>
      <div class="port io-out ro-po" data-port="${escAttr(n.id+':'+p.id)}" data-tip="${escAttr((p.label||p.id)+' · read-only readout')}"><span class="socket${kindClass(p.kind)}"></span></div>
    </div>`;
  }).join('');
  const help=n.help?`<button type="button" class="cl-help" data-clhelp data-noselectdrag data-tip="${escAttr(n.help)}"><i data-lucide="help-circle"></i></button>`:'';
  return `<div class="sh-head ro-head">
      <div class="sh-badge"><i data-lucide="${escAttr(n.icon||'gauge')}"></i></div>
      <div class="sh-id"><div class="sh-name" data-titletext>${esc(n.title)}</div><div class="sh-model">control loop · smart controller</div></div>
      <div class="sh-meta">${help}<div class="sh-online ro-online" data-roonline title="Loop status"></div></div>
    </div>
    <div class="sh-rows ro-rows">
      <div class="cl-grp">Inputs — wire your sensor here</div>${inrows}
      <div class="cl-grp">Readouts — read-only</div>${outrows}
    </div>`;
}
function constantDef(name,t){
  const seed=t.slice('input.constant.'.length); // bool | float | text
  const kindp={k:'Value type',v:seed,kind:'const-kind',
    opts:[{v:'bool',l:'On / off'},{v:'float',l:'Number'},{v:'text',l:'Text'}]};
  const valp={k:'Value',param:'value',kind:'const-val',vkind:seed,v:seed==='bool'?'false':(seed==='text'?'':'0')};
  return {cat:NAME_CAT[name]||'logic',icon:NAME_ICON[name]||'pi',title:name,type:t,implemented:true,live:true,
    props:[kindp,valp],ports:{in:[],out:[{id:'out',label:'Q',kind:seed}]}};
}
function createNode(name,wx,wy,cat){const t=defFor(name,cat);if(!t)return;
  // One Shelly module per device per graph: a device's relays and readouts
  // are single physical channels, so a second instance would bind them
  // twice (the run would reject it). Refuse the drop and select the module
  // already on the canvas so the user finds it.
  if(t.type==='shelly.device'&&t.shelly){
    for(const id in nodes){const d=nodes[id].def;if(d.shelly&&d.shelly.mac===t.shelly.mac){selectOnly(id);return;}}
  }
  // One readout module per device per graph: its readouts are single
  // physical channels, so a second instance would bind them twice (the run
  // would reject it). Refuse the drop and select the existing module.
  if(t.type==='readout.device'&&t.readout){
    for(const id in nodes){const d=nodes[id].def;if(d.readout&&d.readout.id===t.readout.id){selectOnly(id);return;}}
  }
  // One control loop per device per graph: the loop is the device's single
  // driver (the monitor takes an exclusive automatic lock while it runs), so a
  // second one would fight the first over the same unit. Refuse the drop and
  // select the loop already on the canvas.
  if(t.type==='midea.control_loop'){
    const dev=climatePropVal(t,'device','');
    for(const id in nodes){const d=nodes[id].def;
      if(d.type==='midea.control_loop'&&dev&&climatePropVal(d,'device','')===dev){selectOnly(id);return;}}
  }
  const def=JSON.parse(JSON.stringify(t)),slug=name.toLowerCase().replace(/[^a-z0-9]/g,'');
  // idc restarts at 0 per page load, so skip ids a loaded graph occupies.
  let id;do{id=slug+'_'+(++idc);}while(nodes[id]);
  def.id=id;def.title=def.title||name;def.ui={x:snap(wx-107),y:snap(wy-40)};
  GRAPH.nodes.push(def);const nel=buildNode(def);if(!reduceMotion){nel.classList.add('spawn');fxBurst(def.ui.x,def.ui.y,nel.offsetWidth,nel.offsetHeight,def.color);}selectOnly(def.id);renderMinimap();markDirty();}

/* ===== create (drag from library) + delete ===== */
export function attachDrag(it){
  it.addEventListener('pointerdown',ev=>{if(it.classList.contains('inactive')||ev.target.closest('.fav-x'))return;ev.preventDefault();const name=it.dataset.name,cat=it.dataset.cat;S.newDrag={name,cat};
    dragghost.style.setProperty('--gc',CAT[cat].color);dragghost.innerHTML=`<span class="gi"><i data-lucide="${NAME_ICON[name]||CAT[cat].icon}"></i></span>${name}`;
    if(window.lucide)lucide.createIcons();dragghost.classList.add('show');moveGhost(ev);
    try{it.setPointerCapture(ev.pointerId);}catch(_){}
    it._mv=e2=>moveGhost(e2);it._up=e2=>dropNew(e2,it);it._cn=()=>cancelNew(it);
    it.addEventListener('pointermove',it._mv);it.addEventListener('pointerup',it._up);it.addEventListener('pointercancel',it._cn);});
}
export function moveGhost(e){dragghost.style.left=(e.clientX+14)+'px';dragghost.style.top=(e.clientY+10)+'px';}
function unhookNew(it){it.removeEventListener('pointermove',it._mv);it.removeEventListener('pointerup',it._up);it.removeEventListener('pointercancel',it._cn);dragghost.classList.remove('show');}
// pointercancel — the browser claimed the gesture (touch pan on the rail,
// screen edge, etc.). Without this the ghost stuck on screen at its last
// position and S.newDrag stayed stale until the next drag.
function cancelNew(it){unhookNew(it);S.newDrag=null;}
export function dropNew(e,it){unhookNew(it);
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
  if(reduceMotion){el.remove();}else{fxBurst(x,y,w,h,col);el.classList.add('despawn');setTimeout(()=>el.remove(),340);}
  markDirty();}
export function deleteSelected(){if(!selection.size)return;[...selection].forEach(deleteNode);clearSel();renderMinimap();recomputeEndpoints();markRequiredPorts();}
