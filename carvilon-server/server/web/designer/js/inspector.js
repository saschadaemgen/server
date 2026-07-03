// Inspector: the right-hand panel for the single selected node — title,
// card colour, editable properties, and the per-port wire colours.

import { nodes, CAT, PALETTE } from './store.js';
import { setSlider } from './sim.js';
import { findWireTo, findWireFrom, applyEdgeColor } from './wires.js';
import { renderMinimap } from './minimap.js';
import { clearSel } from './selection.js';
import { deleteSelected } from './nodes.js';
import { loadLines, claimedLines } from './gpiolines.js';
import { loadChats } from './telegramchats.js';
import { makeDropdown } from './dropdown.js';

// Custom dropdowns created for the current inspector render; destroyed on
// the next render so none leaves a document listener dangling.
let liveDropdowns=[];

export function openInspector(id){
  const n=nodes[id].def;document.getElementById('insp-cat').textContent=CAT[n.cat].label.toUpperCase();
  document.getElementById('insp-cat').style.color=n.color;
  const ti=document.getElementById('insp-title');ti.value=n.title;
  ti.oninput=()=>{n.title=ti.value;nodes[id].el.querySelector('[data-titletext]').textContent=ti.value;};
  // colors
  const cc=document.getElementById('insp-colors');cc.innerHTML='';
  PALETTE.forEach(col=>{const s=document.createElement('div');s.className='sw'+(col===n.color?' sel':'');s.style.background=col;s.style.setProperty('--swc',col);
    s.onclick=()=>{n.color=col;nodes[id].el.style.setProperty('--cat',col);[...cc.children].forEach(x=>x.classList.remove('sel'));s.classList.add('sel');renderMinimap();};cc.appendChild(s);});
  // props. inspectorOnly props (the GPIO pin options) are edited here but
  // not shown on the node card, so the body .pv index only advances for the
  // props that DO render on the card.
  const pc=document.getElementById('insp-props');
  liveDropdowns.forEach(d=>{try{d.destroy();}catch(e){}});liveDropdowns=[];
  pc.innerHTML='';
  let bodyIdx=-1;
  n.props.forEach(p=>{
    if(!p.inspectorOnly)bodyIdx++;
    const myBody=p.inspectorOnly?-1:bodyIdx;
    const row=document.createElement('div');row.className='iprop';row.innerHTML=`<label>${p.k}</label>`;
    const setBody=v=>{if(myBody<0)return;const pv=nodes[id].el.querySelectorAll('[data-body] .pv')[myBody];if(pv)pv.textContent=v;};
    if(p.kind==='gpio-line'){
      // Pick the physical line from the detected list via the custom
      // dropdown: usable GPIOs up front, system/peripheral lines collapsed
      // behind "Alle anzeigen", search to type instead of scroll. A line
      // another GPIO block claims, or one the system holds, is disabled -
      // each physical line is used at most once.
      const slot=document.createElement('div');slot.className='cv-dd-slot';row.appendChild(slot);pc.appendChild(row);
      loadLines().then(lines=>{
        const claimed=claimedLines(id),cur=p.v,multiChip=new Set(lines.map(l=>l.chip)).size>1;
        const items=lines.map(l=>{
          const takenByOther=claimed.has(l.address)&&l.address!==cur;
          const hint=l.inUse?'belegt':(takenByOther?'vergeben':'');
          return {value:l.address,label:l.name||('Line '+l.offset),
            sub:(multiChip?l.chip:'')+(multiChip&&hint?' · ':'')+hint,
            disabled:(l.inUse||takenByOther)&&l.address!==cur,
            muted:!l.usable&&l.address!==cur};
        });
        // A saved line the host no longer exposes still shows its raw
        // address (and reads as bound) rather than the placeholder, like the
        // old <select> fallback - the value is preserved, only the label was
        // missing.
        if(cur&&!items.some(i=>i.value===cur))items.unshift({value:cur,label:cur,sub:'nicht erkannt'});
        const dd=makeDropdown({value:cur,items,search:true,placeholder:'— Line wählen —',moreLabel:'Alle anzeigen',
          onChange:v=>{p.v=v;const it=items.find(i=>i.value===v);setBody(it?it.label:v);}});
        slot.appendChild(dd.el);liveDropdowns.push(dd);
      });
      return;
    }
    if(p.kind==='enum'){
      const dd=makeDropdown({value:p.v,items:(p.opts||[]).map(o=>({value:o.v,label:o.l})),
        onChange:v=>{p.v=v;const o=(p.opts||[]).find(x=>x.v===v);setBody(o?o.l:v);}});
      row.appendChild(dd.el);liveDropdowns.push(dd);pc.appendChild(row);return;
    }
    if(p.kind==='tg-chat'){
      // Pick the target/source chat from the allowlist (fetched fresh
      // on every inspector open - a chat approved on /a/telegram a
      // moment ago must appear without an editor reload). No claim
      // pool: several blocks may address the same chat.
      const slot=document.createElement('div');slot.className='cv-dd-slot';row.appendChild(slot);pc.appendChild(row);
      loadChats().then(chats=>{
        const cur=p.v;
        const items=chats.map(c=>({value:c.id,label:c.label||c.id,sub:c.label?c.id:''}));
        // A saved chat that is no longer on the allowlist stays visible
        // (the value is preserved), flagged - the run will refuse it.
        if(cur&&!items.some(i=>i.value===cur))items.unshift({value:cur,label:cur,sub:'nicht freigegeben'});
        const dd=makeDropdown({value:cur,items,search:true,placeholder:'— Chat wählen —',
          onChange:v=>{p.v=v;const it=items.find(i=>i.value===v);setBody(it?it.label:v);}});
        slot.appendChild(dd.el);liveDropdowns.push(dd);
      });
      return;
    }
    if(p.kind==='tg-cmd'){
      // Command word, matched trimmed + case-insensitive at runtime.
      // '#' is stripped as typed: it is the channel-ref slot delimiter,
      // and a word containing it would silently bind shorter than shown.
      const inp=document.createElement('input');inp.value=p.v;inp.placeholder='z.B. licht an';inp.spellcheck=false;
      inp.oninput=()=>{if(inp.value.indexOf('#')>=0)inp.value=inp.value.replace(/#/g,'');p.v=inp.value;setBody(p.v);};
      row.appendChild(inp);pc.appendChild(row);return;
    }
    if(p.kind==='mqtt-topic'){
      // Free-text topic. p.v holds the raw topic (no prefix); run.js
      // prefixes "mqtt:" at serialize time, and the card shows the topic.
      const inp=document.createElement('input');inp.value=p.v;inp.placeholder='z.B. haus/eg/flur/taster';inp.spellcheck=false;
      inp.oninput=()=>{p.v=inp.value.trim();setBody(p.v);};
      row.appendChild(inp);pc.appendChild(row);return;
    }
    if(p.kind==='mqtt-kind'){
      // Value-type selector: switching it re-types the engine node to
      // source/sink.channel.<kind> (bool has no suffix), so the run binds
      // the right channel kind. Cosmetic port/live changes are left as-is.
      const dd=makeDropdown({value:p.v,items:(p.opts||[]).map(o=>({value:o.v,label:o.l})),
        onChange:v=>{p.v=v;const o=(p.opts||[]).find(x=>x.v===v);setBody(o?o.l:v);
          const suffix=v==='bool'?'':('.'+v);nodes[id].def.type=p.base+suffix;}});
      row.appendChild(dd.el);liveDropdowns.push(dd);pc.appendChild(row);return;
    }
    if(p.kind==='number'){
      const wrap=document.createElement('div');wrap.className='iprop-num';
      const inp=document.createElement('input');inp.type='number';inp.min='0';inp.value=p.v;inp.className='iprop-numin';
      inp.oninput=()=>{p.v=inp.value;setBody(inp.value);};wrap.appendChild(inp);
      if(p.suffix){const s=document.createElement('span');s.className='iprop-suffix';s.textContent=p.suffix;wrap.appendChild(s);}
      row.appendChild(wrap);pc.appendChild(row);return;
    }
    const inp=document.createElement('input');inp.value=p.v;
    inp.oninput=()=>{p.v=inp.value;setBody(inp.value);};
    row.appendChild(inp);pc.appendChild(row);});
  if(n.control==='slider'){const row=document.createElement('div');row.className='iprop';
    const inp=document.createElement('input');inp.value=n.value.toFixed(1);
    inp.oninput=()=>{const v=parseFloat(inp.value);if(!isNaN(v)){n.value=Math.min(n.max,Math.max(n.min,v));setSlider(id,n.value);}};
    row.innerHTML=`<label>${n.vlabel} (${n.unit})</label>`;row.appendChild(inp);pc.appendChild(row);}
  // per-port wire colors
  const ws=document.getElementById('insp-wires'),wsec=document.getElementById('insp-wires-sec');ws.innerHTML='';
  const rws=[];
  n.ports.in.forEach(p=>{const o=findWireTo(id+':'+p.id);if(o)rws.push({label:p.label,dir:'Input',o});});
  n.ports.out.forEach(p=>{const o=findWireFrom(id+':'+p.id);if(o)rws.push({label:p.label,dir:'Output',o});});
  if(!rws.length){wsec.style.display='none';}
  else{wsec.style.display='';rws.forEach(rw=>{const box=document.createElement('div');box.className='iwire';const dc=rw.dir==='Input'?'var(--cat-input)':'var(--cat-output)';
    box.innerHTML=`<div class="iwl"><span class="dirdot" style="color:${dc};background:${dc}"></span>${rw.dir} · ${rw.label}</div>`;
    const sws=document.createElement('div');sws.className='swatches';
    PALETTE.forEach(col=>{const s=document.createElement('div');s.className='sw'+(col===(rw.o.color||'#34E4EA')?' sel':'');s.style.background=col;s.style.setProperty('--swc',col);
      s.onclick=()=>{applyEdgeColor(rw.o,col);[...sws.children].forEach(x=>x.classList.remove('sel'));s.classList.add('sel');};sws.appendChild(s);});
    box.appendChild(sws);ws.appendChild(box);});}
}
document.getElementById('insp-close').onclick=()=>clearSel();
document.getElementById('insp-del').onclick=()=>deleteSelected();
