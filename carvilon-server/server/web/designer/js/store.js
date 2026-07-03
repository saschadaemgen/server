// Shared store: constants, category metadata, the working graph, the
// live node/wire collections, the cross-module view state, and the
// handful of DOM roots every module reaches for.
//
// This file is the seam that lets the former single-file editor split
// into ES modules without changing behaviour: the few values that
// several modules both read and write (the pan/zoom transform, the
// snap/grid toggles, the in-flight library drag) live on the shared
// `S` object, because a module-local `let` cannot be reassigned across
// an import. Everything else is a plain export.

export const NS = t => document.createElementNS('http://www.w3.org/2000/svg', t);
export const reduceMotion = window.matchMedia('(prefers-reduced-motion: reduce)').matches;
export const GRID = 26;
export function hexRgb(h){h=h.replace('#','');return [parseInt(h.slice(0,2),16),parseInt(h.slice(2,4),16),parseInt(h.slice(4,6),16)];}

// HTML escaping for the template-literal renderers (node cards, tree
// rows, inspector rows). Since persistence, def fields (titles, prop
// values) round-trip through storage as user-editable text and must
// never reach innerHTML raw. esc for text nodes, escAttr for
// attribute values.
export function esc(s){return String(s).replace(/[&<>]/g,m=>({'&':'&amp;','<':'&lt;','>':'&gt;'}[m]));}
export function escAttr(s){return String(s).replace(/[&<>"]/g,m=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[m]));}

export const CAT={input:{color:'#2DD4EF',label:'Input',icon:'log-in'},logic:{color:'#A78BFA',label:'Logic',icon:'git-fork'},
  time:{color:'#F6B23C',label:'Timing',icon:'timer'},memory:{color:'#5B9DFF',label:'Memory',icon:'database'},output:{color:'#43E08A',label:'Output',icon:'zap'}};
export const PALETTE=['#2DD4EF','#43E08A','#5BE0C8','#B8E04A','#F6B23C','#FF8A5B','#FF6B8B','#A78BFA','#5B9DFF','#EAF1F5'];

// Background grid "neuron" animation settings, read live by background.js
// and edited from the toolbar settings popover (settings.js). Persisted to
// localStorage so the choice survives reloads. enabled=off stops the FX;
// beatMs is the tick interval, the others are multipliers (see FX_DEFAULTS).
export const FX_DEFAULTS={enabled:true,rate:6,speed:7,length:12,intensity:0.7,blur:1,gridSize:GRID,twinkle:true,twinkleRate:1,tickEnabled:true,tickRate:0.2,tickIntensity:2.2};
// Preset grid sizes the settings slider snaps between (px, world units).
export const GRID_SIZES=[16,20,26,32,40,52];
export const FX=(function(){const d=Object.assign({},FX_DEFAULTS);try{const s=JSON.parse(localStorage.getItem('cv_grid_fx2')||'null');if(s&&typeof s==='object')for(const k in d)if(typeof s[k]===typeof d[k])d[k]=s[k];}catch(e){}return d;})();
export function saveFX(){try{localStorage.setItem('cv_grid_fx2',JSON.stringify(FX));}catch(e){}}

// The working graph. Starts EMPTY: since persistence the canvas is
// filled exactly once by project.js when the selected/deep-linked
// graph arrives from the API - the former built-in demo template (its
// content lives on as the stored "EG · Flur" graph) caused a visible
// flash of stale nodes on every reload. Node defs use the engine port
// names (out/trig/q/set) so a graph maps straight onto the engine when
// Run executes it.
export const GRAPH={nodes:[],edges:[]};

// Autosave seam: the canvas modules call markDirty() after every
// user-driven graph mutation (create/delete/move/wire/edit); project.js
// registers the actual debounced scheduler here. A property on an
// exported object (not a reassignable import) and defined in store.js,
// which imports nothing, so no module cycles.
export const graphDirty={fn:null};
export function markDirty(){if(graphDirty.fn)graphDirty.fn();}

// Live collections, mutated in place by the canvas modules.
export const nodes={};
export const wires=[];
export const wireByEdge={};
export const selection=new Set();

// Cross-module view/interaction state. These are reassigned from more
// than one module, so they must be object properties (imported value
// bindings are read-only).
// grid is the live world grid pitch (snap step + background dot spacing);
// seeded from the persisted FX.gridSize, edited via the settings popover.
export const S={scale:1,tx:0,ty:0,userAdjusted:false,snapOn:true,gridOn:true,newDrag:null,grid:GRID_SIZES.includes(FX.gridSize)?FX.gridSize:GRID};

export function snap(v){return S.snapOn?Math.round(v/S.grid)*S.grid:v;}

// Shared DOM roots. The module scripts are deferred, so these elements
// already exist in index.html by the time this module evaluates.
export const world=document.getElementById('world');
export const svg=document.getElementById('wires');
export const vp=document.getElementById('viewport');
export const marquee=document.getElementById('marquee');
export const dragghost=document.getElementById('dragghost');
