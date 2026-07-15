// Selection: the set of currently selected node ids, its visual
// reflection on the nodes, and the inspector show/hide that follows a
// single selection.

import { nodes, selection } from './store.js';
import { openInspector } from './inspector.js';
import { resetChannelView } from './shellysettings.js';
import { destroyReadoutHistory } from './readouthistory.js';

export function refreshSelVisual(){for(const id in nodes) nodes[id].el.classList.toggle('selected',selection.has(id));updateContext();}
export function selectOnly(id){selection.clear();if(id)selection.add(id);refreshSelVisual();}
export function toggleSel(id){if(selection.has(id))selection.delete(id);else selection.add(id);refreshSelVisual();}
export function clearSel(){selection.clear();refreshSelVisual();}
export function updateContext(){
  const insp=document.getElementById('inspector');
  if(selection.size===1){openInspector([...selection][0]);insp.classList.add('show');}
  // Hiding the panel must also tear down a mounted history chart - a hidden
  // panel still has an in-flight fetch and a ResizeObserver otherwise.
  else{insp.classList.remove('show');resetChannelView();destroyReadoutHistory();} // back to device view next open
}
export function selEls(){return [...selection].map(id=>nodes[id].el);}
