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

import { nodes, esc, escAttr } from './store.js';
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

// ---- device view: channel picker + the FULL device-level surface (the
// same endpoints and coverage as the /a/devices cockpit; the device's
// own GetConfig tree defines completeness - MQTT/cloud stay read-only,
// destructive actions sit behind confirms, auth changes sync CARVILON's
// stored password in the same action).
function renderDevice(container, nodeId){
  const def = nodes[nodeId].def, chans = (def.shelly && def.shelly.channels) || [];
  container.innerHTML =
    `<div class="si-sec"><div class="si-h">Channels</div><div class="si-chans">`+
    chans.map(c=>`<button type="button" class="si-chan" data-ch="${c.id}"><span class="si-chan-dot" data-cid="${c.id}"></span>CH${c.id+1}<i data-lucide="chevron-right"></i></button>`).join('')+
    `</div></div>`+
    `<div class="si-devsecs"><div class="si-load">Reading device…</div></div>`;
  container.querySelectorAll('[data-ch]').forEach(b=>b.onclick=()=>openChannelSettings(nodeId, Number(b.dataset.ch)));
  if(window.lucide) lucide.createIcons();
  const sid = def.shelly && def.shelly.id;
  const host = container.querySelector('.si-devsecs');
  if(!sid){ host.innerHTML='<div class="si-note">Device not linked.</div>'; return; }
  fetch('shelly/'+sid+'/device',{credentials:'same-origin'}).then(r=>r.ok?r.json():null).then(d=>{
    if(channelView.node!==null) return; // navigated into a channel meanwhile
    if(!d || d.ok===false){ host.innerHTML='<div class="si-load err">'+esc((d&&d.error)||'Device unreachable.')+'</div>'; return; }
    renderDeviceSections(host, sid, d);
  }).catch(()=>{ host.innerHTML='<div class="si-load err">Device unreachable.</div>'; });
}

// collapsible section helper (all start collapsed - the sidebar stays short)
function collSec(title, inner){
  return `<div class="si-sec si-coll"><div class="si-h si-collh">${esc(title)}<i data-lucide="chevron-right"></i></div><div class="si-collb" hidden>${inner}</div></div>`;
}
function roF(k,v){ return field(k, `<span class="si-ro">${esc(v==null||v===''?'—':String(v))}</span>`); }

function renderDeviceSections(host, sid, d){
  const cfg=d.config||{}, sys=cfg.sys||{}, dev=sys.device||{}, loc=sys.location||{}, sntp=sys.sntp||{};
  const w=d.wifiStatus||{}, e=d.ethStatus||{}, mq=cfg.mqtt||{}, cl=cfg.cloud||{}, ble=cfg.ble||{}, ws=cfg.ws||{}, knx=cfg.knx;
  let devHTML =
    field('Device name', txt('sys:device.name', dev.name==null?'':dev.name))+
    field('Eco mode', tog('sys:device.eco_mode', dev.eco_mode===true))+
    field('Discoverable', tog('sys:device.discoverable', dev.discoverable===true))+
    field('Timezone', txt('sys:location.tz', loc.tz==null?'':loc.tz))+
    field('Latitude', num('sys:location.lat', loc.lat==null?'':loc.lat,''))+
    field('Longitude', num('sys:location.lon', loc.lon==null?'':loc.lon,''))+
    field('SNTP server', txt('sys:sntp.server', sntp.server==null?'':sntp.server));
  if(cfg.ui && cfg.ui.idle_brightness!=null) devHTML += field('Panel brightness', num('ui:idle_brightness', cfg.ui.idle_brightness,'%'));
  let connHTML =
    `<div class="si-hint">MQTT + cloud are managed by CARVILON provisioning (read-only).</div>`+
    roF('MQTT server', mq.server)+roF('Topic prefix', mq.topic_prefix)+roF('MQTT on', String(mq.enable===true))+
    roF('Cloud on', String(cl.enable===true))+
    field('Bluetooth (BLE)', tog('ble:enable', ble.enable===true))+
    field('BLE RPC', tog('ble:rpc.enable', !!(ble.rpc&&ble.rpc.enable)))+
    roF('Websocket', ws.enable===true?(ws.server||'enabled'):'disabled');
  if(knx) connHTML += roF('KNX on', String(knx.enable===true))+roF('KNX addr', knx.ia);
  const netHTML =
    roF('Ethernet IP', e.ip)+roF('WiFi SSID', w.ssid||((cfg.wifi&&cfg.wifi.sta)||{}).ssid)+
    roF('WiFi IP', w.sta_ip||w.ip)+roF('WiFi RSSI', w.rssi!=null?w.rssi+' dBm':null);
  const fwHTML = roF('Firmware', dev.fw_id||(d.info&&d.info.fw_id))+
    `<div class="si-f"><span class="si-f-c"><button type="button" class="si-addbtn" data-fwcheck>Check update</button>`+
    `<button type="button" class="si-addbtn" data-reboot>Reboot</button></span></div><div data-fwout></div>`;
  const authHTML =
    `<div class="si-hint">Rotates the password on ALL adopted Shelly devices AND updates CARVILON's stored password in one action; unreachable devices are reported.</div>`+
    field('New password', `<input class="si-i" type="password" data-authpw autocomplete="new-password">`)+
    `<div class="si-f"><span class="si-f-c"><button type="button" class="si-addbtn" data-authset>Apply</button><span class="si-st" data-authst></span></span></div>`;
  host.innerHTML =
    collSec('Device', devHTML)+
    collSec('Firmware & maintenance', fwHTML)+
    collSec('Scripts', `<div data-scripts><div class="si-load">…</div></div>`)+
    collSec('Webhooks', `<div data-hooks><div class="si-load">…</div></div>`)+
    collSec('Auth', authHTML)+
    collSec('Network', netHTML)+
    collSec('Connectivity', connHTML);
  if(window.lucide) lucide.createIcons();
  host.querySelectorAll('.si-collh').forEach(h=>h.onclick=()=>{
    const b=h.parentElement.querySelector('.si-collb'); b.hidden=!b.hidden;
    h.parentElement.classList.toggle('open', !b.hidden);
    if(!b.hidden){
      const sc=b.querySelector('[data-scripts]'); if(sc&&!sc.dataset.loaded) devScripts(sc, sid);
      const hk=b.querySelector('[data-hooks]'); if(hk&&!hk.dataset.loaded) devHooks(hk, sid);
    }
  });
  // device-level apply-on-change: data-k "component:dotted.key"
  host.querySelectorAll('[data-k]').forEach(el=>{
    const full=el.dataset.k, ci=full.indexOf(':'); if(ci<0) return;
    const comp=full.slice(0,ci), key=full.slice(ci+1);
    const st=el.closest('.si-f')&&el.closest('.si-f').querySelector('[data-st]');
    const send=v=>{ if(st){st.textContent='…';st.className='si-st';}
      fetch('shelly/'+sid+'/'+comp+'-config',{method:'POST',headers:{'Content-Type':'application/json'},credentials:'same-origin',body:JSON.stringify({config:{[key]:v}})})
        .then(r=>{ if(st){st.textContent=r.ok?'saved':'failed';st.className='si-st'+(r.ok?' ok':' err');} })
        .catch(()=>{ if(st){st.textContent='failed';st.className='si-st err';} }); };
    if(el.dataset.tog!==undefined){ el.onclick=()=>{ const on=!el.classList.contains('on'); el.classList.toggle('on',on); send(on); }; }
    else if(el.tagName==='SELECT'){ el.onchange=()=>send(el.value); }
    else if(el.type==='number'){ el.onchange=()=>{ if(el.value.trim()==='') return; send(Number(el.value)||0); }; }
    else { el.onchange=()=>send(el.value); }
  });
  const fwb=host.querySelector('[data-fwcheck]'), fwo=host.querySelector('[data-fwout]');
  if(fwb) fwb.onclick=()=>{
    fwb.disabled=true; fwo.innerHTML='<div class="si-load">Checking…</div>';
    fetch('shelly/'+sid+'/fw-check',{credentials:'same-origin'}).then(r=>r.ok?r.json():null).then(x=>{
      fwb.disabled=false;
      if(!x){ fwo.innerHTML='<div class="si-load err">Check failed - device unreachable.</div>'; return; }
      const u=x.updates||{}; const parts=[];
      ['stable','beta'].forEach(stg=>{ if(u[stg]&&u[stg].version) parts.push(`<div class="si-f"><span class="si-f-l">${esc(stg)} ${esc(u[stg].version)}</span><span class="si-f-c"><button type="button" class="si-addbtn" data-up="${stg}">Install</button></span></div>`); });
      fwo.innerHTML = parts.length?parts.join(''):'<div class="si-none">No update available.</div>';
      fwo.querySelectorAll('[data-up]').forEach(bb=>bb.onclick=()=>{
        if(!confirm('Install the '+bb.dataset.up+' firmware now? The device reboots afterwards.')) return;
        bb.disabled=true;
        fetch('shelly/'+sid+'/fw-update',{method:'POST',headers:{'Content-Type':'application/json'},credentials:'same-origin',body:JSON.stringify({stage:bb.dataset.up})})
          .then(r=>{ fwo.innerHTML = r.ok?'<div class="si-none">Update started.</div>':'<div class="si-load err">Update failed to start.</div>'; });
      });
    }).catch(()=>{ fwb.disabled=false; fwo.innerHTML='<div class="si-load err">Check failed.</div>'; });
  };
  const rb=host.querySelector('[data-reboot]');
  if(rb) rb.onclick=()=>{
    if(!confirm('Reboot the device now?')) return;
    rb.disabled=true;
    fetch('shelly/'+sid+'/reboot',{method:'POST',credentials:'same-origin'}).then(()=>{rb.disabled=false;});
  };
  const ab=host.querySelector('[data-authset]');
  if(ab) ab.onclick=()=>{
    const pw=host.querySelector('[data-authpw]').value, st=host.querySelector('[data-authst]');
    if(pw.length<8){ st.textContent='min 8 chars'; st.className='si-st err'; return; }
    if(!confirm('Rotate the password on ALL adopted Shelly devices AND update the stored CARVILON password now?')) return;
    st.textContent='…'; st.className='si-st';
    fetch('shelly/'+sid+'/auth',{method:'POST',headers:{'Content-Type':'application/json'},credentials:'same-origin',body:JSON.stringify({password:pw})})
      .then(r=>r.ok?r.json():Promise.reject())
      .then(d=>{ const fl=(d.failed||[]).length; st.textContent=fl?(d.applied+' ok, '+fl+' failed'):('applied to '+d.applied); st.className='si-st'+(fl?' err':' ok'); })
      .catch(()=>{ st.textContent='failed'; st.className='si-st err'; });
  };
}

function devScripts(hostEl, sid){
  hostEl.dataset.loaded='1';
  fetch('shelly/'+sid+'/scripts',{credentials:'same-origin'}).then(r=>r.ok?r.json():null).then(d=>{
    const list=(d&&d.scripts)||[];
    const reload=()=>{ delete hostEl.dataset.loaded; devScripts(hostEl,sid); };
    hostEl.innerHTML = list.map(s=>
      `<div class="si-srow" data-sid="${s.id}"><div class="si-srow-i"><div class="si-srow-t">${esc(s.name||('script '+s.id))}</div><div class="si-srow-d">${s.running?'running':'stopped'}${s.enable?' · autostart':''}</div></div>`+
      `<button type="button" class="si-del" data-a="${s.running?'stop':'start'}" title="${s.running?'Stop':'Start'}"><i data-lucide="${s.running?'square':'play'}"></i></button>`+
      `<button type="button" class="si-del" data-a="edit" title="Edit"><i data-lucide="pencil"></i></button>`+
      `<button type="button" class="si-del" data-a="delete" title="Delete"><i data-lucide="trash-2"></i></button></div>`+
      `<div class="si-scred" data-ed="${s.id}" hidden></div>`).join('')+
      `<div class="si-times"><input class="si-i" type="text" data-newname placeholder="New script name"><button type="button" class="si-addbtn" data-new><i data-lucide="plus"></i></button></div>`;
    if(window.lucide) lucide.createIcons();
    hostEl.querySelectorAll('.si-srow[data-sid]').forEach(rw=>{
      const id=Number(rw.dataset.sid);
      rw.querySelectorAll('[data-a]').forEach(bb=>bb.onclick=()=>{
        const a=bb.dataset.a;
        if(a==='edit'){
          const ed=hostEl.querySelector(`[data-ed="${id}"]`);
          if(!ed.hidden){ ed.hidden=true; return; }
          ed.hidden=false; ed.innerHTML='<div class="si-load">Loading…</div>';
          fetch('shelly/'+sid+'/script/'+id+'/code',{credentials:'same-origin'}).then(r=>r.ok?r.json():Promise.reject()).then(x=>{
            ed.innerHTML='<textarea class="si-code" spellcheck="false"></textarea><div class="si-times"><button type="button" class="si-addbtn" data-save>Save</button><span class="si-st" data-st></span></div>';
            ed.querySelector('textarea').value=(x&&x.code)||'';
            ed.querySelector('[data-save]').onclick=()=>{
              const st=ed.querySelector('[data-st]'); st.textContent='…'; st.className='si-st';
              fetch('shelly/'+sid+'/script/'+id+'/code',{method:'POST',headers:{'Content-Type':'application/json'},credentials:'same-origin',body:JSON.stringify({code:ed.querySelector('textarea').value})})
                .then(r=>{ st.textContent=r.ok?'saved':'failed'; st.className='si-st'+(r.ok?' ok':' err'); });
            };
          }).catch(()=>{ ed.innerHTML='<div class="si-load err">Code could not be loaded - not editable right now.</div>'; });
          return;
        }
        if(a==='delete' && !confirm('Delete this script permanently?')) return;
        bb.disabled=true;
        fetch('shelly/'+sid+'/script/'+id+'/'+a,{method:'POST',credentials:'same-origin'}).then(r=>{ if(r.ok) reload(); else bb.disabled=false; }).catch(()=>{bb.disabled=false;});
      });
    });
    const nb=hostEl.querySelector('[data-new]');
    if(nb) nb.onclick=()=>{
      const name=hostEl.querySelector('[data-newname]').value.trim(); if(!name) return;
      nb.disabled=true;
      fetch('shelly/'+sid+'/script',{method:'POST',headers:{'Content-Type':'application/json'},credentials:'same-origin',body:JSON.stringify({name})})
        .then(r=>{ if(r.ok) reload(); else nb.disabled=false; }).catch(()=>{nb.disabled=false;});
    };
  }).catch(()=>{ hostEl.innerHTML='<div class="si-load err">Scripts could not be loaded.</div>'; });
}

function devHooks(hostEl, sid){
  hostEl.dataset.loaded='1';
  fetch('shelly/'+sid+'/webhooks',{credentials:'same-origin'}).then(r=>r.ok?r.json():null).then(d=>{
    const hooks=(d&&d.hooks&&d.hooks.hooks)||[], sup=(d&&d.supported)||{}, types=sup.hook_types||(sup.types?Object.keys(sup.types):[]);
    const reload=()=>{ delete hostEl.dataset.loaded; devHooks(hostEl,sid); };
    const form=h=>{
      const ev=(h&&h.event)||'';
      const opts=types.length?types.map(t=>`<option value="${escAttr(t)}"${t===ev?' selected':''}>${esc(t)}</option>`).join(''):`<option value="${escAttr(ev)}" selected>${esc(ev||'event')}</option>`;
      return `<div class="si-scred"><select class="si-i" data-h="event">${opts}</select>`+
        `<input class="si-i" type="number" data-h="cid" value="${h&&h.cid!=null?(Number(h.cid)||0):0}" title="Channel id">`+
        `<input class="si-i" type="text" data-h="name" value="${escAttr((h&&h.name)||'')}" placeholder="Name">`+
        `<textarea class="si-code" style="min-height:44px" data-h="urls" placeholder="URLs, one per line">${esc(((h&&h.urls)||[]).join('\n'))}</textarea>`+
        `<div class="si-times"><button type="button" class="si-addbtn" data-hsave>Save</button><span class="si-st" data-st></span></div></div>`;
    };
    const read=(sc,prevEnable)=>({ event:sc.querySelector('[data-h="event"]').value, cid:Number(sc.querySelector('[data-h="cid"]').value)||0,
      name:sc.querySelector('[data-h="name"]').value, urls:sc.querySelector('[data-h="urls"]').value.split('\n').map(u=>u.trim()).filter(Boolean),
      enable: prevEnable==null ? true : !!prevEnable });
    hostEl.innerHTML = hooks.map(h=>
      `<div class="si-srow" data-wid="${Number(h.id)||0}"><div class="si-srow-i"><div class="si-srow-t">${esc(h.name||('hook '+h.id))}</div><div class="si-srow-d">${esc(h.event||'')}</div></div>`+
      `<button type="button" class="si-del" data-a="edit" title="Edit"><i data-lucide="pencil"></i></button>`+
      `<button type="button" class="si-del" data-a="delete" title="Delete"><i data-lucide="trash-2"></i></button></div>`+
      `<div data-wed="${Number(h.id)||0}" hidden></div>`).join('')+
      `<div class="si-times"><button type="button" class="si-addbtn" data-newhook><i data-lucide="plus"></i>Webhook</button></div><div data-newform hidden></div>`;
    if(window.lucide) lucide.createIcons();
    hostEl.querySelectorAll('.si-srow[data-wid]').forEach(rw=>{
      const wid=Number(rw.dataset.wid), h=hooks.find(x=>x.id===wid);
      rw.querySelectorAll('[data-a]').forEach(bb=>bb.onclick=()=>{
        if(bb.dataset.a==='delete'){
          if(!confirm('Delete this webhook?')) return;
          bb.disabled=true;
          fetch('shelly/'+sid+'/webhook/'+wid+'/delete',{method:'POST',credentials:'same-origin'}).then(()=>reload()).catch(()=>{bb.disabled=false;});
          return;
        }
        const ed=hostEl.querySelector(`[data-wed="${wid}"]`);
        if(!ed.hidden){ ed.hidden=true; return; }
        ed.hidden=false; ed.innerHTML=form(h);
        ed.querySelector('[data-hsave]').onclick=()=>{
          const st=ed.querySelector('[data-st]'); st.textContent='…'; st.className='si-st';
          fetch('shelly/'+sid+'/webhook/'+wid+'/update',{method:'POST',headers:{'Content-Type':'application/json'},credentials:'same-origin',body:JSON.stringify(read(ed, h&&h.enable))})
            .then(r=>{ st.textContent=r.ok?'saved':'failed'; st.className='si-st'+(r.ok?' ok':' err'); if(r.ok) reload(); });
        };
      });
    });
    const nb=hostEl.querySelector('[data-newhook]');
    if(nb) nb.onclick=()=>{
      const nf=hostEl.querySelector('[data-newform]');
      if(!nf.hidden){ nf.hidden=true; return; }
      nf.hidden=false; nf.innerHTML=form(null);
      nf.querySelector('[data-hsave]').onclick=()=>{
        const st=nf.querySelector('[data-st]'); st.textContent='…'; st.className='si-st';
        fetch('shelly/'+sid+'/webhook',{method:'POST',headers:{'Content-Type':'application/json'},credentials:'same-origin',body:JSON.stringify(read(nf))})
          .then(r=>{ st.textContent=r.ok?'created':'failed'; st.className='si-st'+(r.ok?' ok':' err'); if(r.ok) reload(); });
      };
    };
  }).catch(()=>{ hostEl.innerHTML='<div class="si-load err">Webhooks could not be loaded.</div>'; });
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
      field('Power-on default', sel('initial_state', s(sw,'initial_state'), INIT_STATES)) +
      field('Lock input', tog('in_locked', b(sw.in_locked))) +
      field('Reverse output', tog('reverse', b(sw.reverse)))) +
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
        field('Input name', txt('input.name', s(inp,'name'))) +
        field('Input type', sel('input.type', s(inp,'type'), IN_TYPES)) +
        field('Input enabled', tog('input.enable', b(inp.enable))) +
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
