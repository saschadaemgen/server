// makeDropdown — a custom select styled like the editor's cards (dark
// surface, cyan accent, rounded), replacing the native <select> that broke
// the inspector's look. It backs both the GPIO line picker (with a search
// box and a collapsed "advanced" tier for system lines) and the small
// option enums (bias / active level / initial state).
//
// opts:
//   value        current value
//   items        [{value, label, sub, disabled, muted}]
//                 - sub:    secondary right-aligned hint ("belegt", chip)
//                 - disabled: not selectable (occupied / one-use taken)
//                 - muted:  a system/peripheral line, hidden behind moreLabel
//   onChange      (value) => void
//   search        show a filter box (the ~26-line picker)
//   placeholder   shown when nothing is selected
//   moreLabel     if set, muted items collapse behind a "<moreLabel> (N)" row
// returns { el, value (get/set) }.

function esc(s) {
  return String(s).replace(/[&<>"]/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c]));
}

export function makeDropdown(opts) {
  const { items = [], onChange, search = false, placeholder = '—', moreLabel = '' } = opts;
  let value = opts.value || '';
  let query = '', showMore = false;

  const root = document.createElement('div'); root.className = 'cv-dd';
  const btn = document.createElement('button'); btn.type = 'button'; btn.className = 'cv-dd-btn';
  const pop = document.createElement('div'); pop.className = 'cv-dd-pop'; pop.hidden = true;
  root.append(btn, pop);

  const labelFor = v => { const it = items.find(i => i.value === v); return it ? it.label : ''; };

  function renderBtn() {
    const lbl = value ? labelFor(value) : '';
    btn.innerHTML = `<span class="cv-dd-cur${lbl ? '' : ' ph'}">${esc(lbl || placeholder)}</span><span class="cv-dd-chev">▾</span>`;
  }

  function renderList() {
    const list = pop.querySelector('[data-role=list]');
    list.innerHTML = '';
    const q = query.trim().toLowerCase();
    const match = it => !q || it.label.toLowerCase().includes(q) || (it.sub && it.sub.toLowerCase().includes(q));
    const visible = items.filter(match);
    const main = visible.filter(i => !i.muted);
    const extra = visible.filter(i => i.muted);

    const addOpt = it => {
      const o = document.createElement('div');
      o.className = 'cv-dd-opt' + (it.value === value ? ' sel' : '') + (it.disabled ? ' dis' : '') + (it.muted ? ' muted' : '');
      o.innerHTML = `<span class="cv-dd-ol">${esc(it.label)}</span>` + (it.sub ? `<span class="cv-dd-os">${esc(it.sub)}</span>` : '');
      if (!it.disabled) o.onclick = () => { value = it.value; renderBtn(); close(); if (onChange) onChange(value); };
      list.appendChild(o);
    };

    main.forEach(addOpt);
    if (extra.length) {
      if (moreLabel && !showMore && !q) {
        const t = document.createElement('div'); t.className = 'cv-dd-more';
        t.textContent = `${moreLabel} (${extra.length})`;
        t.onclick = () => { showMore = true; renderList(); };
        list.appendChild(t);
      } else {
        extra.forEach(addOpt);
      }
    }
    if (!main.length && !extra.length) {
      const e = document.createElement('div'); e.className = 'cv-dd-empty'; e.textContent = 'Keine Treffer';
      list.appendChild(e);
    }
  }

  function renderPop() {
    pop.innerHTML = '';
    if (search) {
      const s = document.createElement('input'); s.className = 'cv-dd-search'; s.placeholder = 'Suchen…'; s.value = query;
      s.oninput = () => { query = s.value; renderList(); };
      pop.appendChild(s);
      setTimeout(() => s.focus(), 0);
    }
    const list = document.createElement('div'); list.className = 'cv-dd-list'; list.dataset.role = 'list';
    pop.appendChild(list);
    renderList();
  }

  function onDoc(e) { if (!root.contains(e.target)) close(); }
  function open() { pop.hidden = false; renderPop(); document.addEventListener('pointerdown', onDoc, true); }
  function close() { pop.hidden = true; document.removeEventListener('pointerdown', onDoc, true); }

  btn.onclick = () => { pop.hidden ? open() : close(); };
  renderBtn();

  return {
    el: root,
    get value() { return value; },
    set value(v) { value = v; renderBtn(); },
    // destroy releases the document listener (open() registers it, only
    // close() removes it) so a dropdown whose row is torn down - inspector
    // re-render, node delete - does not leave a dangling handler.
    destroy() { close(); },
  };
}
