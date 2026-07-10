// Shelly settings rendered INTO THE INSPECTOR SIDEBAR (the editor's
// established properties panel) - never a modal. The module (device)
// selected shows device-level settings; clicking a channel shows that
// channel's settings. Config is pre-filled from the device and applied
// back over HTTP-RPC (Switch/Input.SetConfig); the weekly schedule uses the
// Schedule.* component. Switching + live readouts stay on MQTT.
//
// Timer (auto on/off, relative seconds) and Weekly Schedule (real weekday
// scheduler on the device's Schedule component) are two DISTINCT sections -
// they must never be conflated.

import { nodes, esc } from './store.js';
import { selectOnly } from './selection.js';

// channelView tracks which channel the inspector is focused on. node===null
// means the device view. `pending` is set ONLY by a channel click, so the
// very next inspector render shows the channel; ANY other open (housing
// click, selecting elsewhere, re-select) falls back to the device view.
const channelView = { node: null, ch: 0 };
let pending = false;
export function resetChannelView(){ channelView.node = null; pending = false; }

// openChannelSettings focuses a channel and opens it in the inspector (by
// selecting the module - selection drives openInspector). Called by the
// faceplate channel click.
export function openChannelSettings(nodeId, ch){
  const nd = nodes[nodeId]; if(!nd || !nd.def.faceplate) return;
  channelView.node = nodeId; channelView.ch = ch; pending = true;
  selectOnly(nodeId); // -> openInspector(nodeId) -> renderShellyInspector
}

const DOW = ['Su','Mo','Tu','We','Th','Fr','Sa']; // index = cron dow (0=Sunday)
const IN_MODES = [['follow','Follow'],['flip','Flip'],['momentary','Momentary'],['detached','Detached']];
const INIT_STATES = [['off','Off'],['on','On'],['restore_last','Restore last'],['match_input','Match input']];
const IN_TYPES = [['switch','Switch'],['button','Button']];

// renderShellyInspector fills the inspector props container for a Shelly
// module: the device view, or a channel view when a channel is focused.
export function renderShellyInspector(container, nodeId){
  const nd = nodes[nodeId]; if(!nd) return;
  if(pending){ pending=false; channelView.node=nodeId; renderChannel(container, nodeId, channelView.ch); }
  else { channelView.node=null; renderDevice(container, nodeId); } // housing / re-select -> device view
}

// ---- device view: pick a channel; device-level settings are a later pass.
function renderDevice(container, nodeId){
  const def = nodes[nodeId].def, chans = (def.shelly && def.shelly.channels) || [];
  container.innerHTML =
    `<div class="si-sec"><div class="si-h">Channels</div><div class="si-chans">`+
    chans.map(c=>`<button type="button" class="si-chan" data-ch="${c.id}"><span class="si-chan-dot" data-cid="${c.id}"></span>CH${c.id+1}<i data-lucide="chevron-right"></i></button>`).join('')+
    `</div></div>`+
    `<div class="si-note">Click a channel to configure it. Scripts, webhooks and PIN come in a later pass.</div>`;
  container.querySelectorAll('[data-ch]').forEach(b=>b.onclick=()=>openChannelSettings(nodeId, Number(b.dataset.ch)));
  if(window.lucide) lucide.createIcons();
}

// ---- channel view ---------------------------------------------------------
function renderChannel(container, nodeId, ch){
  const def = nodes[nodeId].def, sid = def.shelly && def.shelly.id;
  container.innerHTML =
    `<button type="button" class="si-back" data-back><i data-lucide="chevron-left"></i>All channels</button>`+
    `<div class="si-chtitle">CH${ch+1}</div>`+
    `<div class="si-body" data-body><div class="si-load">Reading device…</div></div>`;
  container.querySelector('[data-back]').onclick=()=>{ channelView.node=null; renderDevice(container,nodeId); };
  if(window.lucide) lucide.createIcons();
  if(!sid){ container.querySelector('[data-body]').innerHTML='<div class="si-load err">Device not linked.</div>'; return; }
  const body = container.querySelector('[data-body]');
  Promise.all([
    fetch('shelly/'+sid+'/channel/'+ch,{credentials:'same-origin'}).then(r=>r.ok?r.json():Promise.reject()),
    fetch('shelly/'+sid+'/schedules',{credentials:'same-origin'}).then(r=>r.ok?r.json():{jobs:[]}).catch(()=>({jobs:[]})),
  ]).then(([cfg,sch])=>{
    // guard against a stale response after the user navigated away
    if(channelView.node!==nodeId || channelView.ch!==ch) return;
    renderChannelForm(body, nodeId, ch, sid, cfg.switch||{}, cfg.input, (sch.jobs||[]));
  }).catch(()=>{ body.innerHTML='<div class="si-load err">Device unreachable — settings run over HTTP-RPC; check the device is on the LAN.</div>'; });
}

// setConfig POSTs a single-key partial config and flashes the field status.
function apply(sid, ch, comp, key, val, statusEl){
  const url = 'shelly/'+sid+'/channel/'+ch+'/'+(comp==='input'?'input-config':'switch-config');
  if(statusEl){ statusEl.textContent='…'; statusEl.className='si-st'; }
  fetch(url,{method:'POST',headers:{'Content-Type':'application/json'},credentials:'same-origin',body:JSON.stringify({config:{[key]:val}})})
    .then(r=>{ if(statusEl){ statusEl.textContent=r.ok?'saved':'failed'; statusEl.className='si-st'+(r.ok?' ok':' err'); } })
    .catch(()=>{ if(statusEl){ statusEl.textContent='failed'; statusEl.className='si-st err'; } });
}

function renderChannelForm(body, nodeId, ch, sid, sw, inp, jobs){
  const b = o=>o===true, s = (o,k)=>o&&o[k]!=null?String(o[k]):'', nOf=(o,k)=>o&&o[k]!=null?o[k]:'';
  body.innerHTML =
    section('General',false,
      field('Name', txt('name', s(sw,'name'))) +
      field('Power-on default', sel('initial_state', s(sw,'initial_state'), INIT_STATES))) +
    section('Timer',true,
      `<div class="si-hint">Relative auto on/off after a switch event (seconds, no calendar).</div>`+
      field('Auto-OFF', tog('auto_off', b(sw.auto_off))) +
      field('after', num('auto_off_delay', nOf(sw,'auto_off_delay'),'s')) +
      field('Auto-ON', tog('auto_on', b(sw.auto_on))) +
      field('after', num('auto_on_delay', nOf(sw,'auto_on_delay'),'s'))) +
    section('Protections',true,
      field('Max power', num('power_limit', nOf(sw,'power_limit'),'W')) +
      field('Max voltage', num('voltage_limit', nOf(sw,'voltage_limit'),'V')) +
      field('Undervoltage', num('undervoltage_limit', nOf(sw,'undervoltage_limit'),'V')) +
      field('Max current', num('current_limit', nOf(sw,'current_limit'),'A')) +
      field('Auto-recover V errors', tog('autorecover_voltage_errors', b(sw.autorecover_voltage_errors)))) +
    section('Physical input',true,
      field('Relay mode', sel('in_mode', s(sw,'in_mode'), IN_MODES)) +
      (inp && Object.keys(inp).length ?
        field('Input type', sel('input.type', s(inp,'type'), IN_TYPES)) +
        field('Invert input', tog('input.invert', b(inp.invert)))
        : `<div class="si-none">No physical input.</div>`)) +
    section('Weekly Schedule',true,
      `<div class="si-hint">Runs on the device (local, no internet). On/off at a time on chosen weekdays.</div>`+
      `<div class="si-sched" data-sched></div>`);
  wireFields(body, sid, ch);
  renderSchedule(body.querySelector('[data-sched]'), nodeId, ch, sid, jobs);
  // reflect a rename on the faceplate immediately
  const nameInp = body.querySelector('[data-k="name"]');
  if(nameInp) nameInp.addEventListener('change',()=>{ const nm=nodes[nodeId]&&nodes[nodeId].el.querySelector(`.sh-row[data-ch="${ch}"] .sh-disp-name`); if(nm&&nameInp.value) nm.textContent=nameInp.value; });
  if(window.lucide) lucide.createIcons();
}

// wireFields hooks each config control to an apply-on-change device write.
function wireFields(body, sid, ch){
  body.querySelectorAll('[data-k]').forEach(el=>{
    const k=el.dataset.k, comp=k.indexOf('input.')===0?'input':'switch', key=comp==='input'?k.slice(6):k;
    const st=el.closest('.si-f') && el.closest('.si-f').querySelector('[data-st]');
    if(el.dataset.tog!==undefined){ el.onclick=()=>{ const on=!el.classList.contains('on'); el.classList.toggle('on',on); apply(sid,ch,comp,key,on,st); }; }
    else if(el.tagName==='SELECT'){ el.onchange=()=>apply(sid,ch,comp,key,el.value,st); }
    else if(el.type==='number'){ el.onchange=()=>apply(sid,ch,comp,key,Number(el.value)||0,st); }
    else { el.onchange=()=>apply(sid,ch,comp,key,el.value,st); }
  });
}

// ---- weekly schedule editor ----------------------------------------------
// A schedule row is one weekday set with an optional on-time and off-time.
// The device stores one job per action, so an on+off row is two jobs; the
// editor groups jobs that target this channel by matching weekday set.
function renderSchedule(host, nodeId, ch, sid, jobs){
  const mine = jobs.filter(j=>(j.calls||[]).some(c=>c.method && c.method.toLowerCase()==='switch.set' && c.params && Number(c.params.id)===ch));
  const rows = groupJobs(mine, ch);
  host.innerHTML = rows.map(rowHTML).join('') + addFormHTML();
  host.querySelectorAll('[data-del]').forEach(btn=>btn.onclick=()=>{
    const ids=btn.dataset.del.split(',').map(Number).filter(n=>!isNaN(n));
    btn.disabled=true;
    fetch('shelly/'+sid+'/schedule/delete',{method:'POST',headers:{'Content-Type':'application/json'},credentials:'same-origin',body:JSON.stringify({ids})})
      .then(()=>reloadSchedule(host,nodeId,ch,sid)).catch(()=>{btn.disabled=false;});
  });
  const add=host.querySelector('[data-add]');
  if(add) add.onclick=()=>{
    const days=[...host.querySelectorAll('.si-dow.on')].map(d=>Number(d.dataset.d)).sort((a,b)=>a-b);
    const onT=host.querySelector('[data-on]').value, offT=host.querySelector('[data-off]').value;
    if(!days.length || (!onT && !offT)){ flash(host,'Pick weekdays and a time'); return; }
    const creates=[];
    if(onT) creates.push(mkJob(ch, onT, true, days));
    if(offT) creates.push(mkJob(ch, offT, false, days));
    add.disabled=true;
    Promise.all(creates.map(j=>fetch('shelly/'+sid+'/schedule',{method:'POST',headers:{'Content-Type':'application/json'},credentials:'same-origin',body:JSON.stringify(j)})))
      .then(()=>reloadSchedule(host,nodeId,ch,sid)).catch(()=>{add.disabled=false;flash(host,'Device rejected the schedule');});
  };
  host.querySelectorAll('.si-dow').forEach(d=>d.onclick=()=>{ const on=!d.classList.contains('on'); d.classList.toggle('on',on); d.setAttribute('aria-pressed',on?'true':'false'); });
}
function reloadSchedule(host,nodeId,ch,sid){
  fetch('shelly/'+sid+'/schedules',{credentials:'same-origin'}).then(r=>r.ok?r.json():{jobs:[]}).catch(()=>({jobs:[]}))
    .then(sch=>{ renderSchedule(host,nodeId,ch,sid,sch.jobs||[]); refreshClock(nodeId,ch,(sch.jobs||[])); });
}
function refreshClock(nodeId,ch,jobs){
  const has=jobs.some(j=>(j.calls||[]).some(c=>c.params&&Number(c.params.id)===ch));
  const clk=nodes[nodeId]&&nodes[nodeId].el.querySelector(`.sh-row[data-ch="${ch}"] [data-chclock]`);
  if(clk) clk.hidden=!has;
}
function flash(host,msg){ let m=host.querySelector('.si-sched-msg'); if(!m){ m=document.createElement('div'); m.className='si-sched-msg'; host.appendChild(m);} m.textContent=msg; }

// mkJob builds a Schedule.Create job: "0 {min} {hour} * * {dow}" (no leading
// zeros, integer weekdays) firing switch.set on/off for the channel.
function mkJob(ch, hhmm, on, days){
  const [h,m]=hhmm.split(':').map(x=>parseInt(x,10));
  const dow = days.length===7 ? '*' : days.join(',');
  return { enable:true, timespec:`0 ${m||0} ${h||0} * * ${dow}`, calls:[{ method:'switch.set', params:{ id:ch, on } }] };
}
// groupJobs pairs an on-job and an off-job with the same weekday set into one
// row; leftovers become single-action rows.
function groupJobs(jobs, ch){
  const parsed = jobs.map(j=>{ const t=parseSpec(j.timespec); const call=(j.calls||[]).find(c=>c.params&&Number(c.params.id)===ch); return { id:j.id, hhmm:t.hhmm, daysKey:t.daysKey, days:t.days, on:!!(call&&call.params.on) }; });
  const rows=[]; const used=new Set();
  parsed.forEach(a=>{ if(used.has(a.id)) return;
    if(a.on){ const off=parsed.find(x=>!used.has(x.id) && !x.on && x.daysKey===a.daysKey); if(off){ used.add(a.id); used.add(off.id); rows.push({days:a.days, on:a.hhmm, off:off.hhmm, ids:[a.id,off.id]}); return; } }
    used.add(a.id); rows.push({days:a.days, [a.on?'on':'off']:a.hhmm, ids:[a.id]});
  });
  return rows;
}
function parseSpec(ts){
  const p=String(ts||'').trim().split(/\s+/);
  const h=parseInt(p[2],10)||0, m=parseInt(p[1],10)||0;
  const dowRaw=p[5]||'*';
  const days = dowRaw==='*' ? [0,1,2,3,4,5,6] : dowRaw.split(',').map(x=>parseInt(x,10)).filter(x=>!isNaN(x));
  return { hhmm: pad(h)+':'+pad(m), daysKey: days.slice().sort((a,b)=>a-b).join(','), days };
}
function pad(n){ return String(n).padStart(2,'0'); }
function daysLabel(days){ return days.length===7 ? 'Every day' : days.map(d=>DOW[d]).join(' '); }

function rowHTML(r){
  const parts=[]; if(r.on) parts.push(`<span class="si-on">on ${esc(r.on)}</span>`); if(r.off) parts.push(`<span class="si-off">off ${esc(r.off)}</span>`);
  return `<div class="si-srow"><div class="si-srow-i"><div class="si-srow-t">${parts.join(' · ')}</div><div class="si-srow-d">${esc(daysLabel(r.days))}</div></div>`+
    `<button type="button" class="si-del" data-del="${r.ids.join(',')}" title="Delete schedule"><i data-lucide="trash-2"></i></button></div>`;
}
function addFormHTML(){
  return `<div class="si-add">`+
    `<div class="si-dows">`+DOW.map((d,i)=>`<button type="button" class="si-dow" data-d="${i}" aria-pressed="false">${d}</button>`).join('')+`</div>`+
    `<div class="si-times"><label class="si-tl">On<input type="time" data-on class="si-time"></label><label class="si-tl">Off<input type="time" data-off class="si-time"></label></div>`+
    `<button type="button" class="si-addbtn" data-add><i data-lucide="plus"></i>Add schedule</button></div>`;
}

// ---- small field builders (inspector look) --------------------------------
function section(title,autonomous,inner){ return `<div class="si-sec"><div class="si-h">${esc(title)}${autonomous?`<span class="si-auto" title="Runs on the device without CARVILON"><i data-lucide="cpu"></i>on device</span>`:''}</div>${inner}</div>`; }
function field(label,ctrl){ return `<div class="si-f"><span class="si-f-l">${esc(label)}</span><span class="si-f-c">${ctrl}<span class="si-st" data-st></span></span></div>`; }
function txt(k,v){ return `<input class="si-i" data-k="${k}" type="text" value="${esc(v==null?'':String(v))}">`; }
function num(k,v,suf){ return `<span class="si-num"><input class="si-i si-n" data-k="${k}" type="number" value="${v===''||v==null?'':esc(String(v))}">${suf?`<span class="si-suf">${esc(suf)}</span>`:''}</span>`; }
function sel(k,v,opts){ return `<select class="si-i" data-k="${k}">`+opts.map(o=>`<option value="${o[0]}"${o[0]===v?' selected':''}>${esc(o[1])}</option>`).join('')+`</select>`; }
function tog(k,on){ return `<button type="button" class="si-tog${on?' on':''}" data-k="${k}" data-tog aria-pressed="${on?'true':'false'}"><span class="si-tog-t"><span class="si-tog-th"></span></span></button>`; }

// A click on a channel display opens that channel in the inspector sidebar;
// a click on the module HOUSING (anywhere on the card that is not a channel,
// port or the manual switch) returns to the device view and closes any open
// channel view.
document.addEventListener("click",e=>{
  const disp=e.target.closest("[data-chsettings]");
  if(disp){ const nodeEl=disp.closest(".node"); if(nodeEl) openChannelSettings(nodeEl.dataset.id, Number(disp.dataset.chsettings)); return; }
  const house=e.target.closest(".node-shelly");
  if(house && !e.target.closest(".port,[data-chsw]")){
    const id=house.dataset.id;
    if(channelView.node===id){ // in this module's channel view -> back to device
      channelView.node=null; pending=false;
      const pc=document.getElementById("insp-props"); if(pc) renderDevice(pc, id);
    }
  }
});
