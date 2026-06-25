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
import { engineLine, setEngineLive, focusEngine } from './dock.js';

// The four engine-backed block types; only these participate in a run.
const IMPL = new Set(['input.manual', 'time.staircase', 'logic.or', 'output.lamp']);
// A press is a short true/false pulse so the engine sees a rising edge
// (and a later press re-triggers). >1 tick (100ms) so a tick samples true.
const PULSE_MS = 170;

let running = false;
let es = null;
let liveByEdge = {};
const btnRun = document.getElementById('btn-run');

export function isRunning(){ return running; }

function esc(s){ return String(s).replace(/[&<>]/g, m => ({'&':'&amp;','<':'&lt;','>':'&gt;'}[m])); }

// serializeGraph turns the editor graph into the engine's canonical
// Graph JSON, keeping only the implemented (engine-backed) nodes and the
// edges between them. Port ids are already the engine port names.
function serializeGraph(){
  const out=[], ids=new Set();
  for(const n of GRAPH.nodes){
    if(!n.type || !IMPL.has(n.type)) continue;
    const node={id:n.id, type:n.type};
    if(n.type==='time.staircase') node.params={duration: Number(n.value)||0};
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
    res=await fetch('run',{method:'POST',headers:{'Content-Type':'application/json'},credentials:'same-origin',body:JSON.stringify(graph)});
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

async function stopRun(){
  if(!running) return;
  closeStream();
  exitRunning();
  try{ await fetch('run/stop',{method:'POST',credentials:'same-origin'}); }catch(_){}
}

function enterRunning(){
  running=true; liveByEdge={};
  document.body.classList.add('running');
  setEngineLive(true); paintRunBtn();
  engineLine('<span class="ok">RUN</span> graph started · 100 ms tick');
}
function exitRunning(){
  running=false;
  document.body.classList.remove('running');
  setEngineLive(false); paintRunBtn(); resetVisuals();
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

// applyChanges maps engine signal changes (reported at the destination
// input port) onto the editor: light the feeding wire, pulse it on a
// rising edge, and set the lamp card. Then recompute node-lit state.
function applyChanges(changes){
  for(const c of changes){
    const on=!!(c.value && c.value.b);
    const o=findWireTo(c.node+':'+c.port);
    if(o){ const key=o.from+'>'+o.to; const was=!!liveByEdge[key]; liveByEdge[key]=on; if(on && !was) firePulse(key); }
    const def=nodes[c.node] && nodes[c.node].def;
    if(def && def.type==='output.lamp') setNodeOn(c.node,on,'engine');
  }
  refreshLive();
}
function refreshLive(){
  for(const o of wires) o.g.classList.toggle('live', !!liveByEdge[o.from+'>'+o.to]);
  for(const id in nodes){
    const def=nodes[id].def;
    if(def.type==='output.lamp') continue; // lamp lit handled by setNodeOn
    let lit=false;
    for(const o of wires){ if(o.from.split(':')[0]===id && liveByEdge[o.from+'>'+o.to]){ lit=true; break; } }
    nodes[id].el.classList.toggle('lit',lit);
  }
}
function engineFrameLine(f){
  const parts=(f.changes||[]).map(c=>esc(c.node)+':'+esc(c.port)+'=<span class="'+(c.value&&c.value.b?'ok':'dim')+'">'+(c.value&&c.value.b)+'</span>').join(' · ');
  engineLine('<span class="blue">tick '+f.tick+'</span> <span class="dim">'+f.time_ms+' ms</span>'+(parts?' '+parts:''));
}
function resetVisuals(){
  liveByEdge={};
  for(const o of wires) o.g.classList.remove('live');
  for(const id in nodes){ nodes[id].el.classList.remove('lit'); const def=nodes[id].def; if(def.type==='output.lamp') setNodeOn(id,false,'engine'); }
}

if(btnRun) btnRun.onclick=()=>{ running ? stopRun() : startRun(); };
paintRunBtn();
