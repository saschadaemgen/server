// Inspector: the right-hand panel for the single selected node — title,
// card colour, editable properties, and the per-port wire colours.

import { nodes, CAT, PALETTE } from './store.js';
import { setSlider } from './sim.js';
import { findWireTo, findWireFrom, applyEdgeColor } from './wires.js';
import { renderMinimap } from './minimap.js';
import { clearSel } from './selection.js';
import { deleteSelected } from './nodes.js';
import { loadLines, claimedLines } from './gpiolines.js';

export function openInspector(id){
  const n=nodes[id].def;document.getElementById('insp-cat').textContent=CAT[n.cat].label.toUpperCase();
  document.getElementById('insp-cat').style.color=n.color;
  const ti=document.getElementById('insp-title');ti.value=n.title;
  ti.oninput=()=>{n.title=ti.value;nodes[id].el.querySelector('[data-titletext]').textContent=ti.value;};
  // colors
  const cc=document.getElementById('insp-colors');cc.innerHTML='';
  PALETTE.forEach(col=>{const s=document.createElement('div');s.className='sw'+(col===n.color?' sel':'');s.style.background=col;s.style.setProperty('--swc',col);
    s.onclick=()=>{n.color=col;nodes[id].el.style.setProperty('--cat',col);[...cc.children].forEach(x=>x.classList.remove('sel'));s.classList.add('sel');renderMinimap();};cc.appendChild(s);});
  // props
  const pc=document.getElementById('insp-props');pc.innerHTML='';
  n.props.forEach((p,idx)=>{const row=document.createElement('div');row.className='iprop';row.innerHTML=`<label>${p.k}</label>`;
    const setBody=v=>{const pv=nodes[id].el.querySelectorAll('[data-body] .pv')[idx];if(pv)pv.textContent=v;};
    if(p.kind==='gpio-line'){
      // Pick the physical line from the detected list (Node-RED-style edit
      // panel). A line claimed by another GPIO block, or used by the
      // system, is disabled - each physical line is used at most once.
      const sel=document.createElement('select');sel.className='iprop-sel';
      sel.onchange=()=>{p.v=sel.value;setBody(sel.value);};
      row.appendChild(sel);pc.appendChild(row);
      loadLines().then(lines=>{
        const claimed=claimedLines(id),cur=p.v;sel.innerHTML='';
        const ph=document.createElement('option');ph.value='';ph.textContent='— Line wählen —';sel.appendChild(ph);
        for(const ln of lines){const o=document.createElement('option');o.value=ln.address;
          const takenByOther=claimed.has(ln.address)&&ln.address!==cur;
          o.textContent=`${ln.chip} · ${ln.offset}${ln.name?' · '+ln.name:''}`+(ln.inUse?' (System)':'')+(takenByOther?' (vergeben)':'');
          if((ln.inUse||takenByOther)&&ln.address!==cur)o.disabled=true;
          sel.appendChild(o);}
        if(cur&&!lines.some(l=>l.address===cur)){const o=document.createElement('option');o.value=cur;o.textContent=cur;sel.appendChild(o);}
        sel.value=cur||'';
      });
      return;
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
