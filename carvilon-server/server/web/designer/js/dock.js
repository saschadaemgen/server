// Bottom dock: the SSH/MQTT/TCP/UDP/System/Engine log terminals with
// double-click column splitting and per-column search. The feeds are
// demo/placeholder content; wiring the real engine/SSE feeds is a later
// ticket.

import { reduceMotion, nodes } from './store.js';
import { mountMqttConsole } from './mqttconsole.js';

// ---- live engine feed (driven by run.js during a real run) ----
// While live, the Engine tab shows real stream lines instead of the
// demo POOL feed.
let engineLive=false;
export function setEngineLive(on){engineLive=!!on;}
function dockNow(){const d=new Date(),p=n=>String(n).padStart(2,'0');return p(d.getHours())+':'+p(d.getMinutes())+':'+p(d.getSeconds());}
// engineLine appends one real line to every column of the Engine tab,
// honouring each column's search filter and the 200-line cap.
export function engineLine(inner){
  const host=document.getElementById('term-engine');if(!host)return;
  const html=`<div><span class="t">${dockNow()}</span> ${inner}</div>`;
  host.querySelectorAll('.term-col').forEach(col=>{const el=col.querySelector('.tcol-body');if(!el)return;const stick=el.scrollTop+el.clientHeight>=el.scrollHeight-24;el.insertAdjacentHTML('beforeend',html);const sv=col.querySelector('.tcol-search'),q=sv&&sv.value?sv.value.toLowerCase():'';if(q){const last=el.lastElementChild;if(last&&!last.textContent.toLowerCase().includes(q))last.style.display='none';}while(el.children.length>200)el.removeChild(el.firstChild);if(stick)el.scrollTop=el.scrollHeight;});
}
// focusEngine activates the Engine dock tab so a run's lines are visible.
export function focusEngine(){const tab=document.querySelector('.dock-tab[data-tab="engine"]');if(tab)tab.click();}

(function(){
  const dock=document.getElementById('dock');if(!dock)return;
  const tabs=[...document.querySelectorAll('.dock-tab')];
  const pick=a=>a[Math.random()*a.length|0], rb=()=>Math.random()<.5?'true':'false', ri=n=>Math.random()*n|0;
  const nowt=()=>{const d=new Date(),p=n=>String(n).padStart(2,'0');return p(d.getHours())+':'+p(d.getMinutes())+':'+p(d.getSeconds());};
  const INIT={
    ssh:`<div><span class="ok">●</span> ssh carvilon@miniserver-eg — connected <span class="dim">(aes256-gcm · port 22)</span></div>`+
        `<div><span class="pr">carvilon@eg-flur</span>:<span class="blue">~</span>$ loxctl status</div>`+
        `<div class="dim">miniserver <span class="ok">online</span> · uptime 14d 06:21 · load 0.08</div>`+
        `<div class="dim">tree bus <span class="ok">ok</span> · 42 devices · 0 errors</div>`+
        `<div class="dim">air bus <span class="ok">ok</span> · 18 devices · rssi −61 dBm</div>`+
        `<div><span class="pr">carvilon@eg-flur</span>:<span class="blue">~</span>$ tail -f /var/log/logic.log</div>`,
    tcp:`<div><span class="ok">●</span> tcp listener <span class="dim">0.0.0.0:9090</span> · accepting</div>`+
        `<div><span class="t">${nowt()}</span> <span class="blue">SYN</span> 10.0.0.21:54122 → :9090</div>`+
        `<div><span class="t">${nowt()}</span> <span class="ok">EST</span> conn #4 · mss 1460 · rtt 6ms</div>`+
        `<div class="dim">tx 1.2 kB · rx 884 B · win 64k</div>`,
    udp:`<div><span class="ok">●</span> udp socket <span class="dim">:9091</span> · bound</div>`+
        `<div><span class="t">${nowt()}</span> <span class="blue">RECV</span> 10.0.0.33:5353 · len 142</div>`+
        `<div><span class="t">${nowt()}</span> <span class="amber">▸</span> datagram · discovery beacon</div>`,
    sys:`<div><span class="t">${nowt()}</span> <span class="ok">INFO</span> designer · graph loaded · nodes=3 edges=2</div>`+
        `<div><span class="t">${nowt()}</span> <span class="ok">INFO</span> catalog · 111 blocks · schema 1</div>`+
        `<div><span class="t">${nowt()}</span> <span class="amber">WARN</span> validate · W_DEFAULT_DURATION on 'stair'</div>`,
    engine:`<div><span class="t">${nowt()}</span> <span class="ok">BUILD</span> parse graph … ok (3 nodes, 2 edges)</div>`+
        `<div><span class="t">${nowt()}</span> <span class="ok">BUILD</span> topo sort … ok · boundaries: 1 (time.staircase)</div>`+
        `<div><span class="dim">idle · editing is not live — Activate to deploy</span></div>`,
  };
  const MAXCOLS=4;
  const TLABEL={ssh:'SSH',mqtt:'MQTT',tcp:'TCP',udp:'UDP',sys:'System',engine:'Engine'};
  // how many side-by-side columns each terminal tab is split into (1..MAXCOLS).
  const splits={};tabs.forEach(t=>{splits[t.dataset.tab]=1;});
  // (re)build the columns of one tab: n independent terminals, each with a
  // header bar (which-one label + per-column search + a close × when split)
  // over a log body seeded with the tab's INIT content; the count also shows
  // as the tab's ×N badge.
  function renderPanes(name){
    const host=document.getElementById('term-'+name);if(!host)return;
    // The MQTT tab is a full MQTT client (its own toolbar + live list),
    // not a generic split-column terminal.
    if(name==='mqtt'){mountMqttConsole(host);const tab=tabs.find(t=>t.dataset.tab===name);if(tab)tab.dataset.split='1';return;}
    const n=splits[name]||1,lbl=TLABEL[name]||name;let h='';
    for(let i=0;i<n;i++){
      const cap=n>1?`${lbl} · ${i+1}`:lbl, tip=n>1?`${lbl}-Terminal ${i+1} von ${n}`:`${lbl}-Terminal`;
      h+=`<div class="term-col"><div class="tcol-bar">`
        +`<span class="tcol-label" title="${tip}">${cap}</span>`
        +`<input class="tcol-search" type="search" placeholder="filtern…" title="Diese Spalte durchsuchen" aria-label="${lbl} filtern">`
        +(n>1?`<button class="tcol-x" type="button" title="Dieses Terminal schließen" aria-label="Terminal schließen">×</button>`:``)
        +`</div><div class="tcol-body">${INIT[name]||''}</div></div>`;
    }
    host.innerHTML=h;
    host.querySelectorAll('.tcol-body').forEach(c=>{c.scrollTop=c.scrollHeight;});
    const tab=tabs.find(t=>t.dataset.tab===name);if(tab)tab.dataset.split=String(n);
  }
  tabs.forEach(t=>renderPanes(t.dataset.tab));
  function setTab(name){tabs.forEach(t=>t.classList.toggle('active',t.dataset.tab===name));document.querySelectorAll('.term').forEach(p=>p.classList.toggle('active',p.dataset.pane===name));dock.classList.remove('collapsed');const a=document.getElementById('term-'+name);if(a)a.querySelectorAll('.tcol-body').forEach(c=>{c.scrollTop=c.scrollHeight;});}
  // double-click a tab title adds a side-by-side column (up to 4); a single
  // click just activates the tab. Columns are removed with the × in their
  // header (delegated below) — no wrap-around.
  tabs.forEach(t=>{
    t.onclick=()=>setTab(t.dataset.tab);
    t.ondblclick=()=>{const name=t.dataset.tab;if(name==='mqtt')return;if(splits[name]>=MAXCOLS)return;splits[name]=(splits[name]||1)+1;renderPanes(name);setTab(name);};
  });
  const dockBody=document.getElementById('dock-body');
  // × closes one column of its tab (never below 1).
  dockBody.addEventListener('click',e=>{const x=e.target.closest('.tcol-x');if(!x)return;const term=x.closest('.term');if(!term)return;const name=term.dataset.pane;splits[name]=Math.max(1,(splits[name]||1)-1);renderPanes(name);setTab(name);});
  // per-column search filters that column's lines (case-insensitive).
  dockBody.addEventListener('input',e=>{const s=e.target.closest('.tcol-search');if(!s)return;const body=s.closest('.term-col').querySelector('.tcol-body');if(!body)return;const q=s.value.toLowerCase();body.querySelectorAll(':scope>div').forEach(ln=>{ln.style.display=(!q||ln.textContent.toLowerCase().includes(q))?'':'none';});});
  document.getElementById('dock-toggle').onclick=()=>dock.classList.toggle('collapsed');
  const POOL={
    ssh:()=>`<div><span class="t">[${nowt()}]</span> ${pick(['input.manual <span class="blue">btn:out</span> = <span class="amber">'+rb()+'</span>','time.staircase q=<span class="ok">true</span> · hold 3.0s','output.lamp set=<span class="ok">true</span> · ch DALI 1','heartbeat ok · tree 42/42','keepalive · rtt 7ms'])}</div>`,
    tcp:()=>`<div><span class="t">[${nowt()}]</span> ${pick(['<span class="blue">SYN</span> 10.0.0.'+(20+ri(40))+':'+(49152+ri(16000))+' → :9090','<span class="ok">EST</span> conn #'+(1+ri(9))+' · rtt '+(3+ri(12))+'ms','RX '+(64+ri(1400))+' B · frame ok','TX '+(64+ri(1400))+' B · ack','<span class="amber">FIN</span> conn #'+(1+ri(9))+' · closed'])}</div>`,
    udp:()=>`<div><span class="t">${nowt()}</span> ${pick(['<span class="blue">RECV</span> 10.0.0.'+(20+ri(40))+' · len '+(40+ri(460)),'<span class="ok">SEND</span> broadcast · len '+(40+ri(200)),'<span class="amber">▸</span> datagram <span class="dim">discovery</span>','<span class="err">drop</span> · checksum mismatch'])}</div>`,
    sys:()=>`<div><span class="t">${nowt()}</span> ${pick(['<span class="blue">DBG</span> render · 60 fps · nodes '+Object.keys(nodes).length,'<span class="ok">INFO</span> designer · autosave draft','<span class="ok">INFO</span> catalog · 111 blocks','<span class="amber">WARN</span> validate · W_DEFAULT_DURATION on stair'])}</div>`,
    engine:()=>`<div><span class="t">${nowt()}</span> ${pick(['<span class="blue">SIM</span> tick · btn=0 stair=0 lamp=0','<span class="blue">SIM</span> waiting for input…','<span class="ok">BUILD</span> graph ok · boundaries 1','<span class="dim">editing not live — Activate to deploy</span>'])}</div>`,
  };
  function addLine(name){const host=document.getElementById('term-'+name);if(!host||!POOL[name])return;host.querySelectorAll('.term-col').forEach(col=>{const el=col.querySelector('.tcol-body');if(!el)return;const stick=el.scrollTop+el.clientHeight>=el.scrollHeight-24;el.insertAdjacentHTML('beforeend',POOL[name]());const sv=col.querySelector('.tcol-search'),q=sv&&sv.value?sv.value.toLowerCase():'';if(q){const last=el.lastElementChild;if(last&&!last.textContent.toLowerCase().includes(q))last.style.display='none';}while(el.children.length>200)el.removeChild(el.firstChild);if(stick)el.scrollTop=el.scrollHeight;});}
  if(!reduceMotion)setInterval(()=>{const a=document.querySelector('.dock-tab.active');const name=a?a.dataset.tab:'ssh';if(name==='engine'&&engineLive)return;addLine(name);},2600);
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
