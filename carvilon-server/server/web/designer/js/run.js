// Run: execute the current graph in the real Go engine and mirror the
// live values back into the editor. Run POSTs the canonical graph, opens
// the monitor SSE, and renders each Frame's changes — wires carrying a
// true signal glow, the lamp card lights, and the Engine dock tab shows
// the real tick lines. While running, the on-card button drives the
// engine (input endpoint) instead of the client-side demo sim. Stop
// closes the stream and returns the editor to its idle state.

import { GRAPH, nodes, wires } from './store.js';
import { findWireTo } from './wires.js';
import { firePulse, setNodeOn } from './sim.js';
import { engineLine, focusEngine } from './dock.js';

// The engine-backed block types; only these participate in a run.
const IMPL = new Set(['input.manual', 'time.staircase', 'logic.or', 'output.lamp',
  'source.channel', 'sink.channel',
  'source.channel.float', 'source.channel.text', 'sink.channel.float', 'sink.channel.text']);
// A press is a short true/false pulse so the engine sees a rising edge
// (and a later press re-triggers). >1 tick (100ms) so a tick samples true.
const PULSE_MS = 170;

let running = false;
let es = null;
let liveByEdge = {};
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
function serializeGraph(){
  const out=[], ids=new Set();
  for(const n of GRAPH.nodes){
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
      params[p.param]=v;
    }
    if(Object.keys(params).length) node.params=params;
    out.push(node); ids.add(n.id);
  }
  const edges=[];
  for(const e of GRAPH.edges){
    const fn=e.from.split(':')[0], tn=e.to.split(':')[0];
    if(ids.has(fn) && ids.has(tn)) edges.push({from:e.from, to:e.to});
  }
  return {schema:1, nodes:out, edges};
}

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
  openStream();
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
  focusEngine();
  enterRunning('<span class="ok">RUN</span> mit laufendem Graph wieder verbunden');
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
// the producing node's own output port, so a source shows on its own card.
// Bool: light the feeding wire, pulse on a rising edge, set the lamp card.
// Float/Text: show the number/text on the card (its live value) and mark
// the feeding wire as carrying a value.
function applyChanges(changes){
  for(const c of changes){
    const def=nodes[c.node] && nodes[c.node].def;
    if(!def) continue;
    const v=c.value||{};
    if(v.kind===1 || v.kind===2){ // Float | Text
      setNodeLive(c.node, formatLive(def, v));
      const o=findWireTo(c.node+':'+c.port);
      if(o) liveByEdge[o.from+'>'+o.to]=true;
      continue;
    }
    const on=!!v.b;
    const o=findWireTo(c.node+':'+c.port);
    if(o){ const key=o.from+'>'+o.to; const was=!!liveByEdge[key]; liveByEdge[key]=on; if(on && !was) firePulse(key); }
    if(def.type==='output.lamp'||def.type==='sink.channel') setNodeOn(c.node,on,'engine');
  }
  refreshLive();
}
// formatLive renders a Float/Text value for the card: text verbatim, a
// float tidied to a couple of significant places plus the block's unit.
function formatLive(def,v){
  if(v.kind===2) return esc(String(v.s||''));
  const n=Number(v.f||0), s=Math.abs(n)>=100?n.toFixed(0):n.toFixed(1);
  return esc(s)+(def.unit?(' '+esc(def.unit)):'');
}
function setNodeLive(nodeId,html){
  const nd=nodes[nodeId]; if(!nd) return;
  const el=nd.el.querySelector('[data-live]');
  if(el){ el.innerHTML=html; nd.el.classList.add('haslive'); }
}
function refreshLive(){
  for(const o of wires) o.g.classList.toggle('live', !!liveByEdge[o.from+'>'+o.to]);
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
  liveByEdge={};
  for(const o of wires) o.g.classList.remove('live');
  for(const id in nodes){
    const el=nodes[id].el, def=nodes[id].def;
    el.classList.remove('lit','haslive');
    const lv=el.querySelector('[data-live]'); if(lv) lv.textContent=def.live?(def.unit?('— '+def.unit):'—'):'';
    if(def.type==='output.lamp'||def.type==='sink.channel') setNodeOn(id,false,'engine');
  }
}

if(btnRun) btnRun.onclick=()=>{ running ? stopRun() : startRun(); };
paintRunBtn();
