// Run: execute the current graph in the real Go engine and mirror the
// live values back into the editor. Run POSTs the canonical graph, opens
// the monitor SSE, and renders each Frame's changes — wires carrying a
// true signal glow, the lamp card lights, and the Engine dock tab shows
// the real tick lines. While running, the on-card button drives the
// engine (input endpoint) instead of the client-side demo sim. Stop
// closes the stream and returns the editor to its idle state.

import { GRAPH, nodes, wires, world } from './store.js';
import { findWireTo } from './wires.js';
import { firePulse, setNodeOn } from './sim.js';
import { engineLine, focusEngine } from './dock.js';

// The engine-backed block types; only these participate in a run.
const IMPL = new Set(['input.manual', 'input.toggle',
  'input.constant.bool', 'input.constant.float', 'input.constant.text',
  'time.staircase', 'logic.or', 'output.lamp',
  'source.channel', 'sink.channel',
  'source.channel.float', 'source.channel.text', 'sink.channel.float', 'sink.channel.text']);
// A press is a short true/false pulse so the engine sees a rising edge
// (and a later press re-triggers). >1 tick (100ms) so a tick samples true.
const PULSE_MS = 170;

let running = false;
let es = null;
let liveByEdge = {};
let liveByPort = {}; // "node:port" -> last Value seen this run (both input + output ports)
// shellyMap maps a synthetic engine node (a Shelly module's expanded
// per-channel source/sink) back to its editor module + port, so run-time
// values land on the faceplate. Rebuilt by serializeGraph on every run.
let shellyMap = {};
let activeGraphId = 0; // the open graph id, so a run can be tied to its graph
const btnRun = document.getElementById('btn-run');

export function isRunning(){ return running; }

// setActiveGraph records which graph is open (project.js calls it on
// boot and every switch), so a started run is tagged with its graph id
// and a reload can tell whether the open graph is the running one.
export function setActiveGraph(id){ activeGraphId = Number(id) || 0; }

function esc(s){ return String(s).replace(/[&<>]/g, m => ({'&':'&amp;','<':'&lt;','>':'&gt;'}[m])); }

// serializeGraph turns the editor graph into the engine's canonical
// Graph JSON, keeping only the implemented (engine-backed) nodes and the
// edges between them. Port ids are already the engine port names.
// buildShellyMap (re)builds the synthetic-engine-node -> module-port
// reverse map from the current graph. It is deterministic (the ids are a
// pure function of the module id + channel), so it can be rebuilt on a
// reload/restore to route live values back onto the faceplate without a
// fresh serialize. Every channel's set/out/pwr is mapped unconditionally;
// entries for engine nodes a run did not actually emit are harmless (they
// simply never receive a change).
function buildShellyMap(){
  shellyMap={};
  for(const n of GRAPH.nodes){
    if(n.type!=='shelly.device' || !n.shelly) continue;
    for(const ch of (n.shelly.channels||[])){
      const c=ch.id;
      shellyMap[n.id+'__sw'+c+'_out']={node:n.id,port:'sw'+c+'_out'};
      shellyMap[n.id+'__sw'+c+'_set']={node:n.id,port:'sw'+c+'_set'};
      shellyMap[n.id+'__in'+c]={node:n.id,port:'in'+c};
      shellyMap[n.id+'__disp'+c]={node:n.id,port:'disp'+c};
      if(ch.meter) shellyMap[n.id+'__sw'+c+'_pwr']={node:n.id,port:'sw'+c+'_pwr'};
    }
  }
}
function serializeGraph(){
  const out=[], ids=new Set();
  const portMap={}; // editor "moduleId:port" -> synthetic engine node id
  buildShellyMap();
  for(const n of GRAPH.nodes){
    if(n.type==='shelly.device') continue; // expanded below
    if(!n.type || !IMPL.has(n.type)) continue;
    const node={id:n.id, type:n.type};
    const params={};
    if(n.type==='time.staircase') params.duration=Number(n.value)||0;
    // props tagged with `param` (e.g. a GPIO block's Line -> channel)
    // become engine params. An MQTT Topic field holds the raw topic; the
    // "mqtt:" namespace prefix is added here so the channel resolves to
    // the mqtt: driver (matching gpio:/sys: physical refs). Telegram
    // props hold the raw chat id / command word; the full ref
    // telegram:<role>:<payload>#<node-id> is composed here - the #slot
    // keeps refs unique per block (the run path enforces one physical
    // channel per node, but two send blocks to one chat are the normal
    // case; the driver ignores the slot for routing).
    for(const p of (n.props||[])){
      if(!p.param) continue;
      let v=p.v;
      if(p.kind==='mqtt-topic') v='mqtt:'+(v||'');
      else if(p.kind==='tg-chat') v='telegram:'+(p.mode||'chat')+':'+(v||'')+'#'+n.id;
      else if(p.kind==='tg-cmd') v='telegram:cmd:'+String(v||'').trim()+'#'+n.id;
      else if(p.kind==='const-val'){
        // A Constant's value serializes as the engine `value` param, typed
        // per the current value-type (bool/number/text) so coerceParams
        // maps it to the right Value kind.
        const vk=p.vkind||'float';
        v = vk==='bool' ? (p.v===true||p.v==='true') : (vk==='text' ? String(p.v==null?'':p.v) : (Number(p.v)||0));
      }
      params[p.param]=v;
    }
    // MQTT selector: fold the source's JSON field or the sink's RPC target
    // into the channel address (the mqtt: driver splits on '#'; a colon in
    // the topic is safe because only the first ':' delimits the namespace).
    // Source: "<topic>#<json.path>". Sink Switch.Set: "<topic>#Switch.Set:<n>".
    if(params.channel && (n.props||[]).some(p=>p.kind==='mqtt-topic')){
      const mp=n.props;
      const path=mp.find(p=>p.mqtt==='path'), method=mp.find(p=>p.mqtt==='method'), relay=mp.find(p=>p.mqtt==='relay');
      if(path && String(path.v||'').trim()) params.channel+='#'+String(path.v).trim();
      else if(method && method.v && method.v!=='publish') params.channel+='#'+method.v+':'+String((relay&&relay.v)||'0').trim();
    }
    if(Object.keys(params).length) node.params=params;
    out.push(node); ids.add(n.id);
  }
  // Shelly module expansion: each module becomes per-channel mqtt: nodes.
  // Readout sources (state always; power on metered channels) are emitted
  // even unwired so the faceplate is live like the real device; a relay
  // control sink is emitted ONLY when wired (an unwired sink would write
  // false at tick 0 and force the relay OFF). portMap/shellyMap let the
  // edge rewrite and the live-value routing bridge editor <-> engine.
  const emit=(engId,type,channel,modId,port)=>{ out.push({id:engId,type,params:{channel}}); ids.add(engId); portMap[modId+':'+port]=engId; };
  for(const n of GRAPH.nodes){
    if(n.type!=='shelly.device' || !n.shelly || !n.shelly.prefix) continue;
    const p=n.shelly.prefix;
    for(const ch of (n.shelly.channels||[])){
      const c=ch.id;
      emit(n.id+'__sw'+c+'_out', 'source.channel', 'mqtt:'+p+'/status/switch:'+c+'#output', n.id, 'sw'+c+'_out');
      if(ch.meter) emit(n.id+'__sw'+c+'_pwr', 'source.channel.float', 'mqtt:'+p+'/status/switch:'+c+'#apower', n.id, 'sw'+c+'_pwr');
      emit(n.id+'__in'+c, 'source.channel', 'mqtt:'+p+'/status/input:'+c+'#state', n.id, 'in'+c);
      // display-only: the whole switch:N status payload (no selector) as text,
      // so the faceplate grid shows W/V/A/Hz — NO new graph ports for these.
      emit(n.id+'__disp'+c, 'source.channel.text', 'mqtt:'+p+'/status/switch:'+c, n.id, 'disp'+c);
      const setPort=n.id+':sw'+c+'_set';
      if(GRAPH.edges.some(e=>e.to===setPort)) emit(n.id+'__sw'+c+'_set', 'sink.channel', 'mqtt:'+p+'/rpc#Switch.Set:'+c, n.id, 'sw'+c+'_set');
    }
  }
  const edges=[];
  for(const e of GRAPH.edges){
    // Rewrite an endpoint on a module port to its synthetic engine node
    // (source 'out' / sink 'in'); a non-module endpoint passes through.
    const from = portMap[e.from]!==undefined ? portMap[e.from]+':out' : e.from;
    const to   = portMap[e.to]!==undefined   ? portMap[e.to]+':in'    : e.to;
    const fn=from.split(':')[0], tn=to.split(':')[0];
    if(ids.has(fn) && ids.has(tn)) edges.push({from, to});
  }
  return {schema:1, nodes:out, edges};
}
// shellyPort translates an engine change coordinate to editor space: a
// synthetic Shelly node maps back to its module + port; everything else
// passes through unchanged.
function shellyPort(node,port){ const r=shellyMap[node]; return r ? {node:r.node, port:r.port} : {node, port}; }

function outPortOf(nodeId){ const def=nodes[nodeId]&&nodes[nodeId].def; if(!def||!def.ports.out.length) return null; return def.ports.out[0].id; }

function sendInput(node, port, value){
  if(!running) return;
  fetch('run/input',{method:'POST',headers:{'Content-Type':'application/json'},credentials:'same-origin',body:JSON.stringify({node,port,value})}).catch(()=>{});
}

// pressNode pulses an input.manual node's output (momentary button).
export function pressNode(nodeId){ const port=outPortOf(nodeId); if(!port) return; sendInput(nodeId,port,true); setTimeout(()=>sendInput(nodeId,port,false),PULSE_MS); }
// toggleNode drives a switch-style input to a sustained value.
export function toggleNode(nodeId,on){ const port=outPortOf(nodeId); if(!port) return; sendInput(nodeId,port,on); }

function paintRunBtn(){
  if(!btnRun) return;
  btnRun.innerHTML = running ? '<i data-lucide="square" class="lead"></i>Stop' : '<i data-lucide="play" class="lead"></i>Run';
  btnRun.classList.toggle('running',running);
  btnRun.title = running ? 'Stop the running graph' : 'Run the graph in the engine';
  if(window.lucide) lucide.createIcons();
}

async function startRun(){
  if(running) return;
  const graph=serializeGraph();
  focusEngine();
  if(!graph.nodes.length){ engineLine('<span class="amber">nothing to run — add an implemented block</span>'); return; }
  let res,data;
  try{
    res=await fetch('run?g='+encodeURIComponent(activeGraphId),{method:'POST',headers:{'Content-Type':'application/json'},credentials:'same-origin',body:JSON.stringify(graph)});
    data=await res.json().catch(()=>({}));
  }catch(err){ engineLine('<span class="err">run request failed</span> '+esc(err&&err.message||err)); return; }
  if(!res.ok || !data.ok){
    if(data.issues && data.issues.length){
      engineLine('<span class="err">validation failed — not running</span>');
      data.issues.forEach(i=>engineLine('<span class="err">'+esc(i.code)+'</span> '+esc(i.message)+(i.node_id?' <span class="dim">('+esc(i.node_id)+')</span>':'')));
    }else{
      engineLine('<span class="err">run rejected</span> '+esc(data.error||('HTTP '+res.status)));
    }
    return;
  }
  enterRunning();
  pushHeldInputs();
  openStream();
}

// pushHeldInputs syncs the engine with any held Switch (input.toggle)
// blocks already ON before the run started: the engine's source starts at
// 0, so without this a pre-set switch would read off until toggled again.
// Momentary Push-buttons need no priming.
function pushHeldInputs(){
  for(const n of GRAPH.nodes){
    if(n.type==='input.toggle' && n.on){ const port=outPortOf(n.id); if(port) sendInput(n.id,port,true); }
  }
}

// Exported so the project tree can stop a run cleanly before switching
// graphs (no silent loss of run state on a graph change).
export async function stopRun(){
  if(!running) return;
  closeStream();
  exitRunning();
  try{ await fetch('run/stop',{method:'POST',credentials:'same-origin'}); }catch(_){}
}

function enterRunning(note){
  running=true; liveByEdge={};
  document.body.classList.add('running');
  paintRunBtn();
  engineLine(note || '<span class="ok">RUN</span> graph started · 100 ms tick');
}

// maybeRestoreRun reconnects to a run that is still live server-side
// after a page reload (the run survives the monitor disconnect for a
// grace period). Only restores when the graph now open is the one
// running - a different open graph stays Stop and the orphan run reaps
// itself. project.js calls this once, after the boot graph has loaded.
export async function maybeRestoreRun(){
  if(running) return;
  let d;
  try{
    const r=await fetch('run/status',{credentials:'same-origin'});
    if(!r.ok) return;
    d=await r.json();
  }catch(_){ return; }
  if(!d || !d.running || Number(d.graph_id)!==activeGraphId) return;
  buildShellyMap(); // route Shelly faceplate values after a reload-restore
  focusEngine();
  enterRunning('<span class="ok">RUN</span> reconnected to the running graph');
  openStream();
}
function exitRunning(){
  running=false;
  document.body.classList.remove('running');
  paintRunBtn(); resetVisuals();
  engineLine('<span class="dim">— run stopped —</span>');
}

function openStream(){
  closeStream();
  try{ es=new EventSource('run/monitor'); }
  catch(err){ engineLine('<span class="err">monitor stream failed</span>'); return; }
  es.addEventListener('snapshot',ev=>{ try{ applyChanges(JSON.parse(ev.data).changes||[]); }catch(_){} });
  es.addEventListener('tick',ev=>{ try{ const f=JSON.parse(ev.data); applyChanges(f.changes||[]); engineFrameLine(f); }catch(_){} });
  es.onerror=()=>{ if(running && es && es.readyState===EventSource.CLOSED) stopRun(); };
}
function closeStream(){ if(es){ es.close(); es=null; } }

// applyChanges maps engine signal changes onto the editor. A change is
// reported at the destination input port AND (engine monitor extension) at
// the producing node's own output port, so every port carries its live
// value. We stash each value by "node:port", paint a per-port badge, show
// the running value on source cards, light lamp/sink cards on a bool, and
// fire a travelling pulse on a rising edge into an input.
function applyChanges(changes){
  for(const c of changes){
    // A Shelly module's channels are synthetic engine nodes; map them back
    // onto the module + faceplate port before painting.
    const ep=shellyPort(c.node, c.port);
    const nd=nodes[ep.node]; if(!nd) continue;
    const def=nd.def, v=c.value||{}, key=ep.node+':'+ep.port, port=ep.port;
    const prev=liveByPort[key];
    liveByPort[key]=v;
    if(def.faceplate) paintShelly(nd,port,v); else paintPort(nd,port,v);
    // Source cards echo their own output value (fixes a blank Bool: a
    // digital source now shows 1/0, not nothing).
    if(def.live && isOwnOutput(def,port)) setNodeLive(ep.node, formatLive(def,v));
    // Lamp / sink cards light on their bool input; a Switch (input.toggle)
    // card mirrors the held level on its own output (so it stays in sync
    // after a reload reconnects to a live run).
    if(v.kind===0 && (def.type==='output.lamp'||def.type==='sink.channel')) setNodeOn(ep.node,!!v.b,'engine');
    else if(v.kind===0 && def.type==='input.toggle' && isOwnOutput(def,port)) setNodeOn(ep.node,!!v.b,'engine');
    // A rising edge arriving at an input port sends a spark down its wire.
    if(v.kind===0 && v.b && !(prev&&prev.b)){ const o=findWireTo(key); if(o) firePulse(o.from+'>'+o.to); }
  }
  refreshLive();
}
// isActive reports whether a value is "carrying" for wire/port highlight:
// a true bool, a non-zero number, or a non-empty string.
function isActive(v){ if(!v) return false; if(v.kind===1) return Math.abs(v.f||0)>1e-9; if(v.kind===2) return !!(v.s&&v.s.length); return !!v.b; }
// formatVal renders a compact per-port badge: 1/0 for bool, a tidy number
// for float, a truncated string for text.
function formatVal(v){
  if(!v) return '';
  if(v.kind===1){ const n=Number(v.f||0); return Math.abs(n)>=100?n.toFixed(0):n.toFixed(1); }
  if(v.kind===2){ const s=String(v.s||''); return s.length>10?s.slice(0,9)+'…':s; }
  return v.b?'1':'0';
}
// paintPort writes the live value into a port's badge and marks the port
// active (accent) when it carries a signal.
function paintPort(nd,port,v){
  const pe=nd.el.querySelector(`[data-port="${cssAttr(nd.def.id+':'+port)}"]`); if(!pe) return;
  const slot=pe.querySelector('[data-pval]'); if(slot) slot.textContent=formatVal(v);
  pe.classList.toggle('io-on', isActive(v));
}
function cssAttr(s){ return String(s).replace(/["\\]/g,'\\$&'); }
function isOwnOutput(def,port){ return (def.ports.out||[]).some(p=>p.id===port); }
// fmtW tidies a wattage for the device total.
function fmtW(f){ const n=Number(f||0); return Math.abs(n)>=100?n.toFixed(0):n.toFixed(1); }
// fmtMetric tidies one metering value to dp decimals (— when absent).
function fmtMetric(val,dp){ const n=Number(val); if(val==null||!isFinite(n)) return '—'; return Math.abs(n)>=1000?n.toFixed(0):n.toFixed(dp); }
// paintShelly routes a live channel value onto the device faceplate:
// state -> the row's on/off styling; the whole-payload display source ->
// the W/V/A/Hz grid + device total; input -> the row's input LED. All this
// data is on the status/switch:N payload the module already reads (no
// computation — voltage/current/freq are reported directly by the device).
function paintShelly(nd,port,v){
  const el=nd.el; let m;
  const online=el.querySelector('[data-shonline]'); if(online) online.classList.add('on');
  if((m=/^sw(\d+)_out$/.exec(port))){
    const row=el.querySelector(`.sh-row[data-ch="${m[1]}"]`); if(!row) return;
    const on=v.kind===0?!!v.b:isActive(v); row.classList.toggle('on',on);
    const st=row.querySelector('[data-chstate]'); if(st) st.textContent=on?'on':'off';
  }else if((m=/^disp(\d+)$/.exec(port))){
    const row=el.querySelector(`.sh-row[data-ch="${m[1]}"]`); if(!row) return;
    let o=null; try{ o=JSON.parse(v.s||''); }catch(_){ return; }
    if(!o||typeof o!=='object') return;
    const setm=(k,val,dp)=>{ const e=row.querySelector('[data-'+k+']'); if(e) e.textContent=fmtMetric(val,dp); };
    setm('chw',o.apower,1); setm('chv',o.voltage,0); setm('cha',o.current,2); setm('chhz',o.freq,0);
    row.dataset.w=Number(o.apower||0);
    let total=0; el.querySelectorAll('.sh-row').forEach(r=>{ total+=Number(r.dataset.w||0); });
    const tot=el.querySelector('[data-shtotal]'); if(tot) tot.textContent=fmtW(total);
  }else if((m=/^in(\d+)$/.exec(port))){
    const led=el.querySelector(`.sh-row[data-ch="${m[1]}"] [data-chin]`); if(led) led.classList.toggle('on',v.kind===0?!!v.b:isActive(v));
  }
}
// The faceplate's clickable relay switch drives the real device over MQTT
// (Switch.Set), independent of any run — but only when the relay is NOT
// graph-driven (output exclusivity: a wired relay is owned by the graph,
// its switch inert). The toggle is optimistic; the status readout / live
// feed confirms the real state.
if(world) world.addEventListener('click', ev=>{
  const sw=ev.target.closest('[data-chsw]'); if(!sw) return;
  const nodeEl=sw.closest('.node'); if(!nodeEl) return;
  const nodeId=nodeEl.dataset.id, ch=Number(sw.dataset.chsw), def=nodes[nodeId]&&nodes[nodeId].def;
  if(!def||!def.shelly) return;
  if(wires.some(o=>o.to===nodeId+':sw'+ch+'_set')) return; // graph-driven: inert
  const row=sw.closest('.sh-row'), on=!row.classList.contains('on');
  row.classList.toggle('on',on); const st=row.querySelector('[data-chstate]'); if(st) st.textContent=on?'on':'off';
  fetch('shelly/switch',{method:'POST',headers:{'Content-Type':'application/json'},credentials:'same-origin',
    body:JSON.stringify({prefix:def.shelly.prefix, channel:ch, on})}).catch(()=>{});
});
// formatLive renders a value for the source card's big live readout:
// 1/0 for bool, a tidied number (+unit) for float, the string for text.
function formatLive(def,v){
  if(v.kind===2) return esc(String(v.s||''));
  if(v.kind===0) return v.b?'1':'0';
  const n=Number(v.f||0), s=Math.abs(n)>=100?n.toFixed(0):n.toFixed(1);
  return esc(s)+(def.unit?(' '+esc(def.unit)):'');
}
function setNodeLive(nodeId,html){
  const nd=nodes[nodeId]; if(!nd) return;
  const el=nd.el.querySelector('[data-live]');
  if(el){ el.innerHTML=html; nd.el.classList.add('haslive'); }
}
// refreshLive colours every wire from the value on its source output port:
// active (bright accent) when the signal is a true bool / non-zero number /
// non-empty string, dimmed otherwise (Loxone-style online view).
function refreshLive(){
  for(const o of wires){ const active=isActive(liveByPort[o.from]); o.g.classList.toggle('live',active); liveByEdge[o.from+'>'+o.to]=active; }
  for(const id in nodes){
    const def=nodes[id].def;
    if(def.type==='output.lamp'||def.type==='sink.channel') continue; // lit handled by setNodeOn
    let lit=false;
    for(const o of wires){ if(o.from.split(':')[0]===id && liveByEdge[o.from+'>'+o.to]){ lit=true; break; } }
    nodes[id].el.classList.toggle('lit',lit);
  }
}
function engineFrameLine(f){
  const parts=(f.changes||[]).map(c=>{
    const v=c.value||{}; let val;
    if(v.kind===1) val='<span class="blue">'+esc(fmtNum(v.f))+'</span>';
    else if(v.kind===2) val='<span class="blue">"'+esc(String(v.s||''))+'"</span>';
    else val='<span class="'+(v.b?'ok':'dim')+'">'+(!!v.b)+'</span>';
    return esc(c.node)+':'+esc(c.port)+'='+val;
  }).join(' · ');
  engineLine('<span class="blue">tick '+f.tick+'</span> <span class="dim">'+f.time_ms+' ms</span>'+(parts?' '+parts:''));
}
function fmtNum(n){ n=Number(n||0); return Math.abs(n)>=100?n.toFixed(0):n.toFixed(2); }
function resetVisuals(){
  liveByEdge={}; liveByPort={}; shellyMap={};
  for(const o of wires) o.g.classList.remove('live');
  for(const id in nodes){
    const el=nodes[id].el, def=nodes[id].def;
    el.classList.remove('lit','haslive');
    const lv=el.querySelector('[data-live]'); if(lv) lv.textContent=def.live?(def.unit?('— '+def.unit):'—'):'';
    el.querySelectorAll('[data-pval]').forEach(s=>s.textContent='');
    el.querySelectorAll('.io-row.io-on').forEach(p=>p.classList.remove('io-on'));
    if(def.faceplate){
      // Reset the Shelly faceplate to idle (live values only flow in a run;
      // names/schedule persist — they come from the config path).
      el.querySelectorAll('.sh-row').forEach(r=>{ r.classList.remove('on'); r.dataset.w='';
        r.querySelectorAll('.sh-mv').forEach(mv=>mv.textContent='—');
        const st=r.querySelector('[data-chstate]'); if(st)st.textContent='off';
        const led=r.querySelector('[data-chin]'); if(led)led.classList.remove('on'); });
      const tot=el.querySelector('[data-shtotal]'); if(tot)tot.textContent='—';
      const on=el.querySelector('[data-shonline]'); if(on)on.classList.remove('on');
    }
    if(def.type==='output.lamp'||def.type==='sink.channel') setNodeOn(id,false,'engine');
  }
}

if(btnRun) btnRun.onclick=()=>{ running ? stopRun() : startRun(); };
paintRunBtn();
