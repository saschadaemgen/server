// Toolbar toggles in the topbar: signal-flow animation, snap-to-grid,
// and grid visibility. These flip the shared S.snapOn / S.gridOn flags
// (read by snap() and the background grid).

import { S } from './store.js';
import { drawBG } from './background.js';

const bsg=document.getElementById('btn-signal');bsg.onclick=()=>{document.body.classList.toggle('signals-on');bsg.classList.toggle('on',document.body.classList.contains('signals-on'));};
const bsn=document.getElementById('btn-snap');bsn.onclick=()=>{S.snapOn=!S.snapOn;bsn.classList.toggle('active',S.snapOn);};
const bgr=document.getElementById('btn-grid');bgr.onclick=()=>{S.gridOn=!S.gridOn;bgr.classList.toggle('active',S.gridOn);drawBG();};
