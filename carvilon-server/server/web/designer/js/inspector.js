// Inspector: the right-hand panel for the single selected node — title,
// card colour, editable properties, and the per-port wire colours.

import { nodes, CAT, PALETTE, esc, markDirty } from './store.js';
import { setSlider } from './sim.js';
import { findWireTo, findWireFrom, applyEdgeColor } from './wires.js';
import { renderMinimap } from './minimap.js';
import { clearSel } from './selection.js';
import { deleteSelected } from './nodes.js';
import { loadLines, claimedLines } from './gpiolines.js';
import { loadChats } from './telegramchats.js';
import { makeDropdown } from './dropdown.js';
import { renderShellyInspector } from './shellysettings.js';
import { renderReadoutInspector, destroyReadoutHistory } from './readouthistory.js';

// Custom dropdowns created for the current inspector render; destroyed on
// the next render so none leaves a document listener dangling.
let liveDropdowns=[];

// retypePortKind updates a channel node's single port kind (side 'in' or
// 'out') on both the def and the card socket, so type-checked wiring and
// the kind colour follow a value-type change (MQTT / Constant selectors).
function retypePortKind(id,side,kind){
  const def=nodes[id].def,port=(side==='out'?def.ports.out:def.ports.in)[0];if(!port)return;
  port.kind=kind;
  const pe=nodes[id].el.querySelector(`[data-port="${id+':'+port.id}"]`),sock=pe&&pe.querySelector('.socket');
  if(sock){sock.classList.remove('k-bool','k-float','k-text');sock.classList.add('k-'+kind);}
}

// renderInspHelp shows the module's own description at the top of its settings
// panel: a module has to explain itself where it is configured, not only in a
// hover on the card. The section is built on demand (and reused) so it needs no
// markup of its own, and it renders for every node kind — faceplate modules
// included, which is exactly where the panel is otherwise thinnest. Blocks
// without a description simply don't show it.
function renderInspHelp(n,beforeSec){
  let sec=document.getElementById('insp-help-sec');
  if(!sec){
    sec=document.createElement('div');sec.className='insp-sec';sec.id='insp-help-sec';
    sec.innerHTML='<label>About this module</label><div class="insp-help" id="insp-help"></div>';
    beforeSec.parentNode.insertBefore(sec,beforeSec);
  }
  const body=sec.querySelector('#insp-help');
  // textContent, never innerHTML: the text comes from the catalog and stays
  // inert markup-wise. CSS (white-space:pre-line) keeps the paragraph breaks.
  body.textContent=n.help||'';
  sec.style.display=n.help?'':'none';
}

export function openInspector(id){
  // Any chart the previous selection mounted dies here: its fetches and its
  // ResizeObserver must not outlive the panel render that owned them.
  destroyReadoutHistory();
  const n=nodes[id].def;document.getElementById('insp-cat').textContent=CAT[n.cat].label.toUpperCase();
  document.getElementById('insp-cat').style.color=n.color;
  const ti=document.getElementById('insp-title');ti.value=n.title;
  ti.oninput=()=>{n.title=ti.value;nodes[id].el.querySelector('[data-titletext]').textContent=ti.value;};
  const pc0=document.getElementById('insp-props');
  const colorsSec=document.getElementById('insp-colors').closest('.insp-sec');
  const propsSec=pc0.closest('.insp-sec'), propsLabel=propsSec.querySelector('label');
  const wiresSec=document.getElementById('insp-wires-sec');
  renderInspHelp(n,colorsSec);
  // Shelly module is a FIRST-CLASS node: it keeps every standard behaviour
  // (module colour + per-connection wire colours). ONLY the property LIST is
  // replaced by the device/channel settings, which live here in the sidebar
  // (not a modal).
  // The climate control loop is the exception: it is a faceplate node, but its
  // device is baked (nothing to pick) and it has REAL settings — profile and
  // target VPD. The Shelly device panel would show it an empty channel list and
  // "Device not linked.", leaving those settings unreachable, so it falls
  // through to the generic property list below.
  // A readout DEVICE block is a faceplate node too, and hit the same wall for
  // the same reason (no def.shelly ⇒ "Device not linked."). Its panel is its
  // recorded history (Sensor History H2), so it routes to its own renderer.
  if(n.faceplate&&n.type!=='midea.control_loop'){
    colorsSec.style.display=''; wiresSec.style.display='';
    if(propsLabel) propsLabel.style.display='none';
    renderInspColors(id,n);
    pc0.innerHTML='';
    liveDropdowns.forEach(d=>{try{d.destroy();}catch(e){}});liveDropdowns=[];
    if(n.type==='readout.device') renderReadoutInspector(pc0,id);
    else renderShellyInspector(pc0,id);
    renderInspWires(id,n);
    return;
  }
  colorsSec.style.display=''; wiresSec.style.display=''; if(propsLabel) propsLabel.style.display='';
  renderInspColors(id,n);
  // props. inspectorOnly props (the GPIO pin options) are edited here but
  // not shown on the node card, so the body .pv index only advances for the
  // props that DO render on the card.
  const pc=document.getElementById('insp-props');
  liveDropdowns.forEach(d=>{try{d.destroy();}catch(e){}});liveDropdowns=[];
  pc.innerHTML='';
  let bodyIdx=-1;
  n.props.forEach(p=>{
    // cardOnly props are edited on the block itself (the climate loop's target
    // field / enable switch). An inspector copy would drift from the widget and
    // skip its live send, so they are not listed here.
    if(p.cardOnly)return;
    if(!p.inspectorOnly)bodyIdx++;
    const myBody=p.inspectorOnly?-1:bodyIdx;
    const row=document.createElement('div');row.className='iprop';row.innerHTML=`<label>${esc(p.k)}</label>`;
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
          const hint=l.inUse?'in use':(takenByOther?'taken':'');
          return {value:l.address,label:l.name||('Line '+l.offset),
            sub:(multiChip?l.chip:'')+(multiChip&&hint?' · ':'')+hint,
            disabled:(l.inUse||takenByOther)&&l.address!==cur,
            muted:!l.usable&&l.address!==cur};
        });
        // A saved line the host no longer exposes still shows its raw
        // address (and reads as bound) rather than the placeholder, like the
        // old <select> fallback - the value is preserved, only the label was
        // missing.
        if(cur&&!items.some(i=>i.value===cur))items.unshift({value:cur,label:cur,sub:'not detected'});
        const dd=makeDropdown({value:cur,items,search:true,placeholder:'— Select line —',moreLabel:'Show all',
          onChange:v=>{p.v=v;const it=items.find(i=>i.value===v);setBody(it?it.label:v);}});
        slot.appendChild(dd.el);liveDropdowns.push(dd);
      });
      return;
    }
    if(p.kind==='enum'&&p.mqtt==='method'){
      // MQTT Out mode selector. Switch.Set is bool-only on the driver, so
      // picking it auto-forces the value-type to Bool (retyping the node +
      // input port) - no manual second step, no kind-mismatch red-flash.
      const dd=makeDropdown({value:p.v,items:(p.opts||[]).map(o=>({value:o.v,label:o.l})),
        onChange:v=>{p.v=v;const o=(p.opts||[]).find(x=>x.v===v);setBody(o?o.l:v);
          if(v==='Switch.Set'){
            const def=nodes[id].def,kp=def.props.find(x=>x.kind==='mqtt-kind');
            if(kp&&kp.v!=='bool'){
              kp.v='bool';def.type=kp.base||'sink.channel';retypePortKind(id,'in','bool');
              const nonInsp=def.props.filter(x=>!x.inspectorOnly),ki=nonInsp.indexOf(kp);
              const pv=nodes[id].el.querySelectorAll('[data-body] .pv')[ki];
              if(pv)pv.textContent=(kp.opts.find(o2=>o2.v==='bool')||{}).l||'bool';
              setTimeout(()=>{if(nodes[id])openInspector(id);},0);
            }
          }
          markDirty();}});
      row.appendChild(dd.el);liveDropdowns.push(dd);pc.appendChild(row);return;
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
        if(cur&&!items.some(i=>i.value===cur))items.unshift({value:cur,label:cur,sub:'not allowed'});
        const dd=makeDropdown({value:cur,items,search:true,placeholder:'— Select chat —',
          onChange:v=>{p.v=v;const it=items.find(i=>i.value===v);setBody(it?it.label:v);}});
        slot.appendChild(dd.el);liveDropdowns.push(dd);
      });
      return;
    }
    if(p.kind==='tg-cmd'){
      // Command word, matched trimmed + case-insensitive at runtime.
      // '#' is stripped as typed: it is the channel-ref slot delimiter,
      // and a word containing it would silently bind shorter than shown.
      const inp=document.createElement('input');inp.value=p.v;inp.placeholder='e.g. light on';inp.spellcheck=false;
      inp.oninput=()=>{if(inp.value.indexOf('#')>=0)inp.value=inp.value.replace(/#/g,'');p.v=inp.value;setBody(p.v);};
      row.appendChild(inp);pc.appendChild(row);return;
    }
    if(p.kind==='mqtt-topic'){
      // Free-text topic. p.v holds the raw topic (no prefix); run.js
      // prefixes "mqtt:" at serialize time, and the card shows the topic.
      const inp=document.createElement('input');inp.value=p.v;inp.placeholder='e.g. carvilon/shelly-<mac>/status/switch:0';inp.spellcheck=false;
      inp.oninput=()=>{p.v=inp.value.trim();setBody(p.v);};
      row.appendChild(inp);pc.appendChild(row);return;
    }
    if(p.kind==='mqtt-path'){
      // Optional dot-notation JSON field pulled from the payload (Shelly:
      // output, apower, temperature.tC). Empty = whole payload. '#' is the
      // channel-ref delimiter, so it is stripped as typed.
      const inp=document.createElement('input');inp.value=p.v;inp.placeholder='e.g. output or temperature.tC';inp.spellcheck=false;
      inp.oninput=()=>{if(inp.value.indexOf('#')>=0)inp.value=inp.value.replace(/#/g,'');p.v=inp.value.trim();setBody(p.v);};
      row.appendChild(inp);pc.appendChild(row);return;
    }
    if(p.kind==='mqtt-kind'){
      // Value-type selector: switching it re-types the engine node to
      // source/sink.channel.<kind> (bool has no suffix), so the run binds
      // the right channel kind AND the port kind follows, so type-checked
      // wiring accepts e.g. a bool Switch driving a Switch.Set sink.
      const dd=makeDropdown({value:p.v,items:(p.opts||[]).map(o=>({value:o.v,label:o.l})),
        onChange:v=>{p.v=v;const o=(p.opts||[]).find(x=>x.v===v);setBody(o?o.l:v);
          const def=nodes[id].def,suffix=v==='bool'?'':('.'+v);def.type=p.base+suffix;
          retypePortKind(id,p.base==='source.channel'?'out':'in',v);markDirty();}});
      row.appendChild(dd.el);liveDropdowns.push(dd);pc.appendChild(row);return;
    }
    if(p.kind==='const-kind'){
      // Constant value-type selector: re-type the node to
      // input.constant.<kind>, swap the value editor, and follow the port
      // kind. Re-open (deferred) rebuilds the props with the right editor.
      const dd=makeDropdown({value:p.v,items:(p.opts||[]).map(o=>({value:o.v,label:o.l})),
        onChange:v=>{p.v=v;const o=(p.opts||[]).find(x=>x.v===v);setBody(o?o.l:v);
          const def=nodes[id].def;def.type='input.constant.'+v;
          const valp=def.props.find(x=>x.kind==='const-val');
          if(valp){valp.vkind=v;valp.v=(v==='bool'?'false':(v==='text'?'':'0'));
            // keep the card's value cell in sync with the reset value
            const pvs=nodes[id].el.querySelectorAll('[data-body] .pv');
            if(pvs[1])pvs[1].textContent=(v==='bool'?'Off':valp.v);}
          retypePortKind(id,'out',v);markDirty();setTimeout(()=>{if(nodes[id])openInspector(id);},0);}});
      row.appendChild(dd.el);liveDropdowns.push(dd);pc.appendChild(row);return;
    }
    if(p.kind==='const-val'){
      // The Constant's held value, edited per its current kind: an On/Off
      // selector (bool), a number field (float), or a text field.
      const vk=p.vkind||'float';
      if(vk==='bool'){
        const dd=makeDropdown({value:p.v,items:[{value:'true',label:'On'},{value:'false',label:'Off'}],
          onChange:v=>{p.v=v;setBody(v==='true'?'On':'Off');}});
        row.appendChild(dd.el);liveDropdowns.push(dd);pc.appendChild(row);return;
      }
      const inp=document.createElement('input');if(vk==='float')inp.type='number';
      inp.value=p.v;inp.spellcheck=false;inp.placeholder=vk==='text'?'e.g. hello':'e.g. 21.5';
      inp.oninput=()=>{p.v=inp.value;setBody(inp.value);};
      row.appendChild(inp);pc.appendChild(row);return;
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
    row.innerHTML=`<label>${esc(n.vlabel)} (${esc(n.unit)})</label>`;row.appendChild(inp);pc.appendChild(row);}
  renderInspWires(id,n);
}
// renderInspColors renders the standard module-colour swatches (every node,
// Shelly included, can be recoloured).
function renderInspColors(id,n){
  const cc=document.getElementById('insp-colors');cc.innerHTML='';
  PALETTE.forEach(col=>{const s=document.createElement('div');s.className='sw'+(col===n.color?' sel':'');s.style.background=col;s.style.setProperty('--swc',col);
    s.onclick=()=>{n.color=col;nodes[id].el.style.setProperty('--cat',col);[...cc.children].forEach(x=>x.classList.remove('sel'));s.classList.add('sel');renderMinimap();};cc.appendChild(s);});
}
// renderInspWires renders the standard per-connection wire-colour pickers
// for every wired port (Shelly module included — its faceplate wires are
// coloured like any other node's).
function renderInspWires(id,n){
  const ws=document.getElementById('insp-wires'),wsec=document.getElementById('insp-wires-sec');ws.innerHTML='';
  const rws=[];
  n.ports.in.forEach(p=>{const o=findWireTo(id+':'+p.id);if(o)rws.push({label:p.label,dir:'Input',o});});
  n.ports.out.forEach(p=>{const o=findWireFrom(id+':'+p.id);if(o)rws.push({label:p.label,dir:'Output',o});});
  if(!rws.length){wsec.style.display='none';return;}
  wsec.style.display='';rws.forEach(rw=>{const box=document.createElement('div');box.className='iwire';const dc=rw.dir==='Input'?'var(--cat-input)':'var(--cat-output)';
    box.innerHTML=`<div class="iwl"><span class="dirdot" style="color:${dc};background:${dc}"></span>${rw.dir} · ${esc(rw.label)}</div>`;
    const sws=document.createElement('div');sws.className='swatches';
    PALETTE.forEach(col=>{const s=document.createElement('div');s.className='sw'+(col===(rw.o.color||'#34E4EA')?' sel':'');s.style.background=col;s.style.setProperty('--swc',col);
      s.onclick=()=>{applyEdgeColor(rw.o,col);[...sws.children].forEach(x=>x.classList.remove('sel'));s.classList.add('sel');};sws.appendChild(s);});
    box.appendChild(sws);ws.appendChild(box);});
}
document.getElementById('insp-close').onclick=()=>clearSel();
document.getElementById('insp-del').onclick=()=>deleteSelected();
