// Nodes: building the visual node cards, the per-category default
// shapes for dropped blocks, the library drag-ghost / drop-to-create
// flow, and node deletion. The canvas starts empty; project.js builds
// the persisted graph via buildNode once it arrives from the API.

import { CAT, GRAPH, nodes, wires, wireByEdge, world, dragghost, S, snap, reduceMotion, selection, markDirty, esc, escAttr } from './store.js';
import { selectOnly, clearSel } from './selection.js';
import { renderMinimap } from './minimap.js';
import { recomputeEndpoints } from './wires.js';
import { NAME_ICON, NAME_CAT, NAME_TYPE, NAME_CHANNEL, NAME_UNIT, NAME_INPUTS, NAME_OUTPUTS, NAME_SHELLY } from './palette.js';

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
  const c=CAT[n.cat]||(CAT[n.cat]={color:'#7f8c99',label:String(n.cat||'?').toUpperCase(),icon:'box'});
  if(n.color==null)n.color=c.color;normPorts(n);
  const el=document.createElement('div');
  el.className='node'+(n.faceplate?' node-shelly':'');el.dataset.id=n.id;el.style.left=n.ui.x+'px';el.style.top=n.ui.y+'px';el.style.setProperty('--cat',n.color);
  if(n.faceplate){
    el.innerHTML=`<div class="node-accent"></div>`+shellyFaceplateHTML(n);
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
  if(n.faceplate)loadShellyOverview(n.id,(n.shelly||{}).id);
  if(window.lucide)lucide.createIcons();return el;
}

// loadShellyOverview fills the faceplate from the device (one HTTP-RPC round
// trip): the real per-channel NAMES (Switch.GetConfig.name) onto each
// display, and the schedule clock icon on any channel with an on-board
// weekly schedule. Best-effort: an unreachable device just leaves the CH-N
// placeholders and no clocks (config runs over HTTP-RPC, independent of the
// MQTT link).
async function loadShellyOverview(nodeId,storeId){
  if(!storeId)return;
  try{
    const r=await fetch('shelly/'+storeId+'/overview',{credentials:'same-origin'});
    if(!r.ok)return;
    const d=await r.json(),nd=nodes[nodeId];if(!nd)return;
    for(const k in (d.names||{})){const nm=nd.el.querySelector(`.sh-row[data-ch="${Number(k)}"] [data-chname]`);if(nm&&d.names[k])nm.textContent=d.names[k];}
    applyShellySchedule(nd,d.jobs||[]);
  }catch(_){/* device unreachable: placeholders stay, no error */}
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
  const sh=n.shelly||{}, model=sh.model||'';
  const metric=(k,u)=>`<div class="sh-m"><span class="sh-mv" data-${k}>—</span><span class="sh-ml">${u}</span></div>`;
  const rows=(n.ports.in.filter(p=>p.srole==='relay')).map(rp=>{
    const c=rp.ch, outs=n.ports.out.filter(p=>p.ch===c);
    const poH=outs.map(p=>`<div class="sh-orow"><span class="sh-plabel">${esc(SHELLY_PLABEL[p.srole]||p.label||p.id)}</span><div class="port io-out sh-po" data-port="${escAttr(n.id+':'+p.id)}" data-tip="${escAttr(SHELLY_PTIP[p.srole]||p.label||'')}"><span class="socket${kindClass(p.kind)}"></span></div></div>`).join('');
    return `<div class="sh-row" data-ch="${c}">
      <div class="sh-inport"><div class="port io-in sh-pin" data-port="${escAttr(n.id+':'+rp.id)}" data-tip="${escAttr(SHELLY_PTIP.relay)}"><span class="socket${kindClass(rp.kind)}"></span></div><span class="sh-plabel">${esc(SHELLY_PLABEL.relay)}</span></div>
      <div class="sh-disp" data-chsettings="${c}" data-noselectdrag title="Channel settings">
        <div class="sh-disp-top"><span class="sh-disp-name" data-chname>CH${c+1}</span><span class="sh-in-led" data-chin title="Physical input state"></span><span class="sh-clock" data-chclock hidden><i data-lucide="clock"></i></span></div>
        <div class="sh-grid">${metric('chw','W')}${metric('chv','V')}${metric('cha','A')}${metric('chhz','Hz')}</div>
        <div class="sh-disp-foot"><span class="sh-disp-state" data-chstate>off</span><span class="sh-next" data-chnext></span></div>
      </div>
      <div class="sh-div"></div>
      <button type="button" class="sh-sw" data-chsw="${c}" data-noselectdrag title="Toggle relay"><span class="sh-track"><span class="sh-thumb"></span></span></button>
      <div class="sh-pout">${poH}</div>
    </div>`;
  }).join('');
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
      // A Shelly relay control is single-use: once driven, it renders "in
      // use" (and the wiring layer refuses a second driver). On a faceplate,
      // the clickable switch for that channel goes inert (graph owns it).
      pe.classList.toggle('io-driven',p.srole==='relay'&&wired);
      if(p.srole==='relay'){const sw=nd.el.querySelector(`[data-chsw="${p.ch}"]`);if(sw)sw.classList.toggle('sh-locked',wired);}
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
    const n=ch.id, lab='CH'+(n+1);
    inp.push({id:'sw'+n+'_set',label:lab,kind:'bool',srole:'relay',ch:n});
    outp.push({id:'sw'+n+'_out',label:'State',kind:'bool',srole:'state',ch:n});
    if(ch.meter)outp.push({id:'sw'+n+'_pwr',label:'Power',kind:'float',srole:'power',ch:n});
    outp.push({id:'in'+n,label:'Input',kind:'bool',srole:'input',ch:n});
  }
  return {cat:'shelly',icon:NAME_ICON[name]||'toggle-right',title:name,type:'shelly.device',implemented:true,live:false,props:[],faceplate:true,
    shelly:{id:s.id,mac:s.mac,prefix:s.prefix,model:s.model,name:s.name,channels:chans},
    ports:{in:inp,out:outp}};
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
