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

export const CAT={input:{color:'#2DD4EF',label:'Input',icon:'log-in'},logic:{color:'#A78BFA',label:'Logic',icon:'git-fork'},
  time:{color:'#F6B23C',label:'Timing',icon:'timer'},memory:{color:'#5B9DFF',label:'Memory',icon:'database'},output:{color:'#43E08A',label:'Output',icon:'zap'}};
export const PALETTE=['#2DD4EF','#43E08A','#F6B23C','#A78BFA','#5B9DFF','#FF6B8B','#EAF1F5'];

export const GRAPH={
  nodes:[
    {id:'btn1',cat:'input',icon:'circle-dot',title:'Push-button',ui:{x:78,y:156},
      props:[{k:'Input',v:'I1',accent:true}],ports:{in:[],out:[{id:'Q',label:'Q'}]},control:'press'},
    {id:'stair1',cat:'time',icon:'timer',title:'Staircase light',ui:{x:442,y:208},
      props:[{k:'Mode',v:'Pulse'}],ports:{in:[{id:'Tr',label:'Tr'}],out:[{id:'Q',label:'Q'}]},control:'slider',value:3,min:1,max:10,step:.5,unit:'s',vlabel:'Hold time'},
    {id:'lamp1',cat:'output',icon:'lightbulb',title:'Lamp',ui:{x:806,y:166},
      props:[{k:'Output',v:'Q3',accent:true},{k:'Channel',v:'DALI 1'}],ports:{in:[{id:'AI',label:'AI'}],out:[]},control:'switch',on:false}
  ],
  edges:[{from:'btn1:Q',to:'stair1:Tr'},{from:'stair1:Q',to:'lamp1:AI'}]
};

// Live collections, mutated in place by the canvas modules.
export const nodes={};
export const wires=[];
export const wireByEdge={};
export const selection=new Set();

// Cross-module view/interaction state. These are reassigned from more
// than one module, so they must be object properties (imported value
// bindings are read-only).
export const S={scale:1,tx:0,ty:0,userAdjusted:false,snapOn:true,gridOn:true,newDrag:null};

export function snap(v){return S.snapOn?Math.round(v/GRID)*GRID:v;}

// Shared DOM roots. The module scripts are deferred, so these elements
// already exist in index.html by the time this module evaluates.
export const world=document.getElementById('world');
export const svg=document.getElementById('wires');
export const vp=document.getElementById('viewport');
export const marquee=document.getElementById('marquee');
export const dragghost=document.getElementById('dragghost');
