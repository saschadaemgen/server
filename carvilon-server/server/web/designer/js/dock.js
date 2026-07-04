// Bottom dock: the log terminals. SSH is a real terminal (xterm.js over
// a WebSocket to a local shell PTY or an outbound SSH client, sshterm.js),
// MQTT a real broker client, System Log streams the server's real journal
// (syslog.js), and the Engine tab shows only real engine events fed by
// run.js. TCP/UDP are placeholders until step 2 of the terminal track:
// they show an honest empty state, no feed. SSH is the only
// multi-instance tab: clicking its active tab (or double-clicking it)
// adds another terminal, up to four side by side, each its own
// independent session. MQTT, System Log, Engine and the TCP/UDP
// placeholders are single-instance.

import { nodes } from './store.js';
import { mountMqttConsole } from './mqttconsole.js';
import { mountSysLog, startSysLog } from './syslog.js';
import { mountSshPane } from './sshterm.js';

function dockNow(){const d=new Date(),p=n=>String(n).padStart(2,'0');return p(d.getHours())+':'+p(d.getMinutes())+':'+p(d.getSeconds());}
// engineLine appends one real line to the Engine tab, honouring the
// column's search filter and the 200-line cap. The first real line
// replaces the idle placeholder.
export function engineLine(inner){
  const host=document.getElementById('term-engine');if(!host)return;
  const html=`<div><span class="t">${dockNow()}</span> ${inner}</div>`;
  host.querySelectorAll('.term-col').forEach(col=>{const el=col.querySelector('.tcol-body');if(!el)return;const idle=el.querySelector('.idle-note');if(idle)idle.remove();const stick=el.scrollTop+el.clientHeight>=el.scrollHeight-24;el.insertAdjacentHTML('beforeend',html);const sv=col.querySelector('.tcol-search'),q=sv&&sv.value?sv.value.toLowerCase():'';if(q){const last=el.lastElementChild;if(last&&!last.textContent.toLowerCase().includes(q))last.style.display='none';}while(el.children.length>200)el.removeChild(el.firstChild);if(stick)el.scrollTop=el.scrollHeight;});
}
// focusEngine activates the Engine dock tab so a run's lines are visible.
export function focusEngine(){const tab=document.querySelector('.dock-tab[data-tab="engine"]');if(tab)tab.click();}

(function(){
  const dock=document.getElementById('dock');if(!dock)return;
  const tabs=[...document.querySelectorAll('.dock-tab')];
  const nowt=()=>{const d=new Date(),p=n=>String(n).padStart(2,'0');return p(d.getHours())+':'+p(d.getMinutes())+':'+p(d.getSeconds());};
  // TCP/UDP show only their honest empty state until the real consoles
  // arrive with terminal-track step 2 — no demo feed anywhere anymore.
  const INIT={
    tcp:`<div class="dim idle-note">Noch keine TCP-Konsole — kommt mit Terminal-Track Schritt 2.</div>`,
    udp:`<div class="dim idle-note">Noch keine UDP-Konsole — kommt mit Terminal-Track Schritt 2.</div>`,
    engine:`<div class="dim idle-note">idle — kein Run aktiv</div>`,
  };
  // SSH mounts real xterm terminals (below), so it has no INIT seed.
  const MAXCOLS=4;
  const TLABEL={ssh:'SSH',mqtt:'MQTT',tcp:'TCP',udp:'UDP',sys:'System',engine:'Engine'};
  // how many side-by-side columns each terminal tab is split into (1..MAXCOLS).
  const splits={};tabs.forEach(t=>{splits[t.dataset.tab]=1;});

  // ---- SSH: the one multi-instance tab ----
  // Each SSH column is a real xterm terminal with its own session. Panes
  // are managed incrementally so adding/closing one never disturbs the
  // live sessions in the others (a full re-render would kill them).
  const sshHost=document.getElementById('term-ssh');
  let sshPanes=[]; // [{col, ctrl}]
  function updateSshUI(){
    if(sshHost)sshHost.classList.toggle('ssh-host-solo',sshPanes.length<=1);
    const tab=tabs.find(t=>t.dataset.tab==='ssh');if(tab)tab.dataset.split=String(Math.max(1,sshPanes.length));
  }
  function addSshPane(){
    if(!sshHost||sshPanes.length>=MAXCOLS)return;
    const col=document.createElement('div');col.className='term-col ssh-col';sshHost.appendChild(col);
    const entry={col,ctrl:null};
    entry.ctrl=mountSshPane(col,{closable:true,onClose:()=>removeSshPane(entry)});
    sshPanes.push(entry);updateSshUI();
  }
  function removeSshPane(entry){
    if(sshPanes.length<=1)return; // never below one terminal
    try{entry.ctrl&&entry.ctrl.dispose();}catch(_){/* already gone */}
    entry.col.remove();sshPanes=sshPanes.filter(e=>e!==entry);updateSshUI();
  }
  function initSshPanes(){if(sshPanes.length===0)addSshPane();}

  // (re)build the columns of one tab: n independent terminals, each with a
  // header bar (label + per-column search) over a log body seeded with the
  // tab's INIT content. SSH, MQTT and System Log have their own mounts.
  function renderPanes(name){
    const host=document.getElementById('term-'+name);if(!host)return;
    if(name==='ssh'){initSshPanes();updateSshUI();return;}
    if(name==='mqtt'){mountMqttConsole(host);const tab=tabs.find(t=>t.dataset.tab===name);if(tab)tab.dataset.split='1';return;}
    if(name==='sys'){mountSysLog(host);const tab=tabs.find(t=>t.dataset.tab===name);if(tab)tab.dataset.split='1';return;}
    const lbl=TLABEL[name]||name;
    const h=`<div class="term-col"><div class="tcol-bar">`
      +`<span class="tcol-label" title="${lbl}-Konsole">${lbl}</span>`
      +`<input class="tcol-search" type="search" placeholder="filtern…" title="Diese Spalte durchsuchen" aria-label="${lbl} filtern">`
      +`</div><div class="tcol-body">${INIT[name]||''}</div></div>`;
    host.innerHTML=h;
    host.querySelectorAll('.tcol-body').forEach(c=>{c.scrollTop=c.scrollHeight;});
    const tab=tabs.find(t=>t.dataset.tab===name);if(tab)tab.dataset.split='1';
  }
  tabs.forEach(t=>renderPanes(t.dataset.tab));
  function setTab(name){tabs.forEach(t=>t.classList.toggle('active',t.dataset.tab===name));document.querySelectorAll('.term').forEach(p=>p.classList.toggle('active',p.dataset.pane===name));dock.classList.remove('collapsed');if(name==='sys')startSysLog();if(name==='ssh')window.dispatchEvent(new Event('resize'));const a=document.getElementById('term-'+name);if(a)a.querySelectorAll('.tcol-body').forEach(c=>{c.scrollTop=c.scrollHeight;});}
  // Clicking the already-active SSH tab (or double-clicking it) adds another
  // terminal, up to four; a plain click just activates the tab. Every other
  // tab is single-instance.
  tabs.forEach(t=>{
    t.onclick=()=>{const name=t.dataset.tab;if(name==='ssh'&&t.classList.contains('active')){addSshPane();return;}setTab(name);};
    t.ondblclick=()=>{if(t.dataset.tab==='ssh')addSshPane();};
  });
  const dockBody=document.getElementById('dock-body');
  // per-column search filters that column's lines (case-insensitive) — for
  // the line-based tabs; SSH terminals are real xterm and have no search.
  dockBody.addEventListener('input',e=>{const s=e.target.closest('.tcol-search');if(!s)return;const body=s.closest('.term-col').querySelector('.tcol-body');if(!body)return;const q=s.value.toLowerCase();body.querySelectorAll(':scope>div').forEach(ln=>{ln.style.display=(!q||ln.textContent.toLowerCase().includes(q))?'':'none';});});
  document.getElementById('dock-toggle').onclick=()=>dock.classList.toggle('collapsed');

  // ---- resizable height ----
  // The divider above the dock bar drags the dock height; the stage is a
  // fullscreen canvas behind the dock overlay, so it takes the remaining
  // room by itself. xterm panes refit through their own ResizeObserver
  // (sshterm.js) — live SSH sessions only get a resize frame, nothing
  // reconnects. The applied height goes through --dock-h so the
  // .collapsed rule (height:0) keeps winning while collapsed.
  const DOCKH_KEY='cv_dock_h',DOCKH_DEF=210,DOCKH_MIN=64;
  // keep at least 160px of stage under the 54px topbar; 34px dock bar.
  const maxDockH=()=>Math.max(DOCKH_MIN,innerHeight-54-160-34);
  function applyDockH(h){const hh=Math.round(Math.max(DOCKH_MIN,Math.min(maxDockH(),h)));dock.style.setProperty('--dock-h',hh+'px');return hh;}
  let dockH=parseInt(localStorage.getItem(DOCKH_KEY)||'',10);
  if(!Number.isFinite(dockH)||dockH<=0)dockH=DOCKH_DEF;
  dockH=applyDockH(dockH);
  const saveDockH=()=>{try{localStorage.setItem(DOCKH_KEY,String(dockH));}catch(_){/* private mode */}};
  const grip=document.getElementById('dock-resize');
  if(grip){
    let startY=0,startH=0,raf=0,want=0,dragging=false;
    grip.addEventListener('pointerdown',e=>{
      if(e.button!==0)return;e.preventDefault();
      dragging=true;startY=e.clientY;
      startH=dock.classList.contains('collapsed')?0:dockBody.getBoundingClientRect().height;
      try{grip.setPointerCapture(e.pointerId);}catch(_){/* stale pointer */}
      dock.classList.add('resizing');document.body.classList.add('dock-resizing');
    });
    grip.addEventListener('pointermove',e=>{
      if(!dragging)return;
      want=startH+(startY-e.clientY);
      // pulling the divider up out of the collapsed state opens the dock
      if(dock.classList.contains('collapsed')&&want>24)dock.classList.remove('collapsed');
      if(!raf)raf=requestAnimationFrame(()=>{raf=0;dockH=applyDockH(want);});
    });
    const endDrag=()=>{if(!dragging)return;dragging=false;dock.classList.remove('resizing');document.body.classList.remove('dock-resizing');saveDockH();};
    grip.addEventListener('pointerup',endDrag);
    grip.addEventListener('pointercancel',endDrag);
    grip.addEventListener('dblclick',()=>{dock.classList.remove('collapsed');dockH=applyDockH(DOCKH_DEF);saveDockH();});
    // window shrink clamps the applied height but keeps the wish, so a
    // grown window restores it.
    addEventListener('resize',()=>applyDockH(dockH));
  }
  setInterval(()=>{const c=document.getElementById('st-clock');if(c)c.textContent=nowt();const n=document.getElementById('st-nodes');if(n)n.textContent=Object.keys(nodes).length;},1000);
  // Replace the placeholder host label with the real host (Pi model / distro),
  // fetched once on load. The status dot stays as the connection indicator;
  // the kernel goes into a tooltip.
  fetch('host',{credentials:'same-origin'}).then(r=>r.ok?r.json():null).then(h=>{
    const el=document.getElementById('st-host'); if(!el||!h) return;
    const parts=h.model?[h.model,h.os]:[h.os,h.arch];
    if(h.ram)parts.push(h.ram+' RAM');
    const label=parts.filter(Boolean).join(' · ');
    if(label){el.textContent=label;if(h.kernel)el.title='Kernel '+h.kernel;}
  }).catch(()=>{});
})();
