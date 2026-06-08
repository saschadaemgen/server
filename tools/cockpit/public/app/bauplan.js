// Bauplan — datengetriebenes System-Diagramm (React Flow + dagre Auto-Layout)
// mit Cluster-Boxen, animierten Kanten und Szenario-Playback.
import { React, XYFlow, dagre } from "/vendor/lib.js";
import { html, useState, useMemo, useEffect, useRef, cx, Icon } from "/app/util.js";

const { ReactFlow, Background, BackgroundVariant, Controls, MiniMap, Handle, Position, MarkerType } = XYFlow;

const NODE_W = 216;
const NODE_H = 66;

const KIND_COLOR = { control: "#60a5fa", media: "#34d399", data: "#94a3b8" };
const KIND_LABEL = { control: "Control", media: "Media", data: "Data" };

// Compound layout: each node is parented to its group cluster, so dagre keeps
// group members together and returns clean, non-overlapping cluster boxes.
function layout(map) {
  const g = new dagre.graphlib.Graph({ compound: true });
  g.setGraph({ rankdir: "LR", nodesep: 30, ranksep: 74, marginx: 40, marginy: 40, ranker: "tight-tree" });
  g.setDefaultEdgeLabel(() => ({}));
  map.groups.forEach((gr) => g.setNode("cluster:" + gr.id, { label: gr.label }));
  map.nodes.forEach((n) => {
    g.setNode(n.id, { width: NODE_W, height: NODE_H });
    if (g.hasNode("cluster:" + n.group)) g.setParent(n.id, "cluster:" + n.group);
  });
  map.edges.forEach((e) => g.setEdge(e.from, e.to));
  dagre.layout(g);

  const pos = {};
  map.nodes.forEach((n) => {
    const p = g.node(n.id);
    pos[n.id] = { x: p.x - NODE_W / 2, y: p.y - NODE_H / 2 };
  });
  const LBL = 22; // headroom for the cluster label
  const clusters = map.groups
    .map((gr) => {
      const c = g.node("cluster:" + gr.id);
      if (!c || !isFinite(c.x)) return null;
      return { ...gr, x: c.x - c.width / 2, y: c.y - c.height / 2 - LBL, w: c.width, h: c.height + LBL };
    })
    .filter(Boolean);
  return { pos, clusters };
}

function CNode({ data }) {
  return html`<div
    class=${cx("cnode", data.active && "cnode--active", data.current && "cnode--current", data.dim && "cnode--dim")}
    style=${{ "--g": data.color }}
    title=${data.role || ""}
  >
    <${Handle} type="target" position=${Position.Left} className="cnode__h" isConnectable=${false} />
    <div class="cnode__label">${data.label}</div>
    ${data.ports && data.ports.length
      ? html`<div class="cnode__ports">${data.ports.map((p) => html`<span key=${p}>${p}</span>`)}</div>`
      : null}
    <${Handle} type="source" position=${Position.Right} className="cnode__h" isConnectable=${false} />
  <//>`;
}

function GBox({ data }) {
  return html`<div class="gbox" style=${{ width: data.w + "px", height: data.h + "px", "--g": data.color }}>
    <span class="gbox__label" style=${{ color: data.color }}>${data.label}</span>
  <//>`;
}

const nodeTypes = { cnode: CNode, gbox: GBox };

// Fit the view once the custom nodes have been measured (the `fitView` prop
// alone runs before measurement and zooms into a subset).
function FitView() {
  const initialized = XYFlow.useNodesInitialized();
  const rf = XYFlow.useReactFlow();
  useEffect(() => {
    if (!initialized) return;
    // instant fit (no animation — avoids mid-animation flashes); fit again once
    // the flex container has settled.
    rf.fitView({ padding: 0.14 });
    const t = setTimeout(() => rf.fitView({ padding: 0.14 }), 150);
    return () => clearTimeout(t);
  }, [initialized]);
  return null;
}

export function Bauplan() {
  const map = window.SYSTEM_MAP;
  if (!map) return html`<div class="md-status md-status--error">system-map.js fehlt.</div>`;

  const { pos, clusters } = useMemo(() => layout(map), []);
  const groupColor = useMemo(() => Object.fromEntries(map.groups.map((g) => [g.id, g.color])), []);

  const [scenarioId, setScenarioId] = useState(null);
  const [step, setStep] = useState(0);
  const [playing, setPlaying] = useState(false);

  const scenario = useMemo(() => map.scenarios.find((s) => s.id === scenarioId) || null, [scenarioId]);

  // cumulative highlight up to the current step
  const hl = useMemo(() => {
    const nodes = new Set();
    const edges = new Set();
    let curNode = null,
      curEdge = null;
    if (scenario) {
      for (let i = 0; i <= step && i < scenario.steps.length; i++) {
        const st = scenario.steps[i];
        if (st.node) nodes.add(st.node);
        if (st.edge) edges.add(st.edge.join("__"));
        if (i === step) {
          curNode = st.node || null;
          curEdge = st.edge ? st.edge.join("__") : null;
        }
      }
    }
    return { nodes, edges, curNode, curEdge };
  }, [scenario, step]);

  // playback timer
  useEffect(() => {
    if (!playing || !scenario) return;
    if (step >= scenario.steps.length - 1) {
      setPlaying(false);
      return;
    }
    const t = setTimeout(() => setStep((s) => s + 1), 1600);
    return () => clearTimeout(t);
  }, [playing, step, scenario]);

  const rfNodes = useMemo(() => {
    const gNodes = clusters.map((b) => ({
      id: "group:" + b.id,
      type: "gbox",
      position: { x: b.x, y: b.y },
      data: b,
      draggable: false,
      selectable: false,
      connectable: false,
      zIndex: 0,
    }));
    const cNodes = map.nodes.map((n) => {
      const active = !scenario || hl.nodes.has(n.id);
      return {
        id: n.id,
        type: "cnode",
        position: pos[n.id],
        connectable: false,
        zIndex: 1,
        style: { width: NODE_W + "px" },
        data: {
          label: n.label,
          role: n.role,
          ports: n.ports,
          color: groupColor[n.group],
          active: scenario ? hl.nodes.has(n.id) : false,
          current: hl.curNode === n.id,
          dim: scenario ? !active : false,
        },
      };
    });
    return [...gNodes, ...cNodes];
  }, [clusters, pos, scenario, hl, groupColor]);

  const rfEdges = useMemo(() => {
    return map.edges.map((e) => {
      const id = e.from + "__" + e.to;
      const onPath = scenario ? hl.edges.has(id) : false;
      const isCurrent = hl.curEdge === id;
      const dim = scenario && !onPath;
      const base = KIND_COLOR[e.kind] || "#94a3b8";
      const color = onPath ? scenario.color : base;
      return {
        id,
        source: e.from,
        target: e.to,
        label: e.label,
        animated: onPath || (!scenario && e.kind === "media"),
        markerEnd: { type: MarkerType.ArrowClosed, color, width: 16, height: 16 },
        labelShowBg: true,
        labelBgPadding: [4, 2],
        labelBgBorderRadius: 4,
        labelBgStyle: { fill: "#0c1322", fillOpacity: dim ? 0.15 : 0.85 },
        labelStyle: { fill: dim ? "#33415580" : "#9fb3c8", fontSize: 10.5, fontWeight: 500 },
        style: {
          stroke: color,
          strokeWidth: isCurrent ? 3.4 : onPath ? 2.4 : 1.5,
          opacity: dim ? 0.1 : 1,
        },
        zIndex: onPath ? 5 : 2,
      };
    });
  }, [scenario, hl]);

  function pick(id) {
    if (scenarioId === id) {
      setScenarioId(null);
      setPlaying(false);
      setStep(0);
    } else {
      setScenarioId(id);
      setStep(0);
      setPlaying(true);
    }
  }

  const atEnd = scenario && step >= scenario.steps.length - 1;
  const curText = scenario ? scenario.steps[step] && scenario.steps[step].text : null;

  return html`<div class="bauplan">
    <div class="bauplan__bar">
      <div class="bauplan__scen">
        <span class="bauplan__barlabel">Szenario:</span>
        ${map.scenarios.map(
          (s) => html`<button
            key=${s.id}
            class=${cx("chip", scenarioId === s.id && "chip--on")}
            style=${scenarioId === s.id ? { "--c": s.color, borderColor: s.color, color: s.color } : { "--c": s.color }}
            onClick=${() => pick(s.id)}
          >
            <span class="chip__dot" style=${{ background: s.color }}></span>${s.label}
          </button>`,
        )}
        ${scenario
          ? html`<button class="chip chip--ghost" onClick=${() => pick(scenarioId)}>✕ Reset</button>`
          : null}
      </div>
      <div class="bauplan__legend">
        ${Object.keys(KIND_COLOR).map(
          (k) => html`<span key=${k} class="lg"><i style=${{ background: KIND_COLOR[k] }}></i>${KIND_LABEL[k]}</span>`,
        )}
      </div>
    </div>

    <div class="bauplan__canvas">
      <${ReactFlow}
        nodes=${rfNodes}
        edges=${rfEdges}
        nodeTypes=${nodeTypes}
        fitView
        fitViewOptions=${{ padding: 0.18 }}
        minZoom=${0.2}
        maxZoom=${2.5}
        nodesConnectable=${false}
        elementsSelectable=${true}
        proOptions=${{ hideAttribution: true }}
      >
        <${FitView} />
        <${Background} variant=${BackgroundVariant.Dots} gap=${22} size=${1} color="#1e2a3d" />
        <${MiniMap} pannable zoomable nodeColor=${(n) => (n.type === "gbox" ? "transparent" : n.data?.color || "#475569")} maskColor="#0a0f1acc" />
        <${Controls} showInteractive=${false} />
      <//>

      ${scenario
        ? html`<div class="playback">
            <div class="playback__head">
              <span class="playback__title" style=${{ color: scenario.color }}>
                <span class="chip__dot" style=${{ background: scenario.color }}></span>${scenario.label}
              </span>
              <span class="playback__count">Schritt ${step + 1} / ${scenario.steps.length}</span>
            </div>
            <div class="playback__text">${curText}</div>
            <div class="playback__progress">
              <i style=${{ width: ((step + 1) / scenario.steps.length) * 100 + "%", background: scenario.color }}></i>
            </div>
            <div class="playback__ctrls">
              <button class="pbtn" disabled=${step === 0} onClick=${() => { setPlaying(false); setStep((s) => Math.max(0, s - 1)); }}><${Icon} name="prev" size=${16} /></button>
              <button class="pbtn pbtn--main" onClick=${() => { if (atEnd) { setStep(0); setPlaying(true); } else setPlaying((p) => !p); }}>
                <${Icon} name=${playing ? "pause" : "play"} size=${16} />
              </button>
              <button class="pbtn" disabled=${atEnd} onClick=${() => { setPlaying(false); setStep((s) => Math.min(scenario.steps.length - 1, s + 1)); }}><${Icon} name="next" size=${16} /></button>
              <button class="pbtn" onClick=${() => { setPlaying(false); setStep(0); }}><${Icon} name="reset" size=${16} /></button>
            </div>
          </div>`
        : html`<div class="bauplan__hint">${map.meta?.note || "Szenario wählen, um den Weg Schritt für Schritt zu sehen."}</div>`}
    </div>
  </div>`;
}
