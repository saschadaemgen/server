# CARVILON Logic Editor — `web/designer/`

A standalone **Vite + Svelte Flow (`@xyflow/svelte`)** app that reproduces the
approved cyber-look logic editor as a real, runnable project (no static HTML).

```bash
cd web/designer
npm install
npm run dev        # http://localhost:5173/a/designer/
npm run build      # -> web/designer/dist/ (base = /a/designer/)
```

Later the built `dist/` is served by the carvilon server under `/a/designer/`
(go:embed) and embedded via iframe in the admin — **separate CC ticket.** The
only requirement that touches us is `vite.config.js` → `base: '/a/designer/'`.

## Layout

```
src/
  main.js               mount App
  app.css               theme tokens + Svelte Flow overrides
  App.svelte            shell: topbar, palette, <SvelteFlow>, issues panel, DnD, undo/redo
  lib/
    catalog.json        DATA CONTRACT — 111 blocks (server delivers this later)
    graph.json          DATA CONTRACT — canonical demo graph (ParseGraph/Build shape)
    categories.js       category labels/colours + port-kind colour/shape
    store.svelte.js     reactive state, catalog load, isValidConnection, undo/redo, toCanonical()
    BlockNode.svelte    custom node (typed ports, category header, params)
    SignalEdge.svelte   custom edge (cyan, drop-shadow glow, signal-flow dashes)
    BgCanvas.svelte     background on its own GPU canvas, own rAF, decoupled from re-render
    Palette.svelte      data-driven palette (grouped by category, collapsible, draggable)
```

## Data contracts (must stay 1:1 with the Go descriptor)

- **`catalog.json`** — `{ schema, blocks:[{ type, category, title, inputs[], outputs[], params[], delay_boundary }] }`.
  `kind ∈ bool|float|text`. The four canonical types (`input.manual`, `logic.or`,
  `time.staircase`, `output.lamp`) are byte-exact; the rest fill the palette
  (design-phase only). Loaded via `fetch(BASE_URL + 'catalog.json')` with the
  bundled import as fallback (`loadCatalog()`).
- **`graph.json`** — `{ schema, nodes:[{id,type,params,ui}], edges:[{from:"node:port",to:"node:port"}] }`.
  `ui` is position-only. `toCanonical()` in the store re-emits exactly this shape
  for a future editor → server export. I1/Q3/DALI-1 bindings are modelled as a
  `point` param on input/output blocks, not a hardware address.

## Implemented (beyond the preview)

- **Typed ports** — colour + shape per `kind` (bool=circle/blue, float=diamond/amber, text=square/violet).
- **`isValidConnection`** — kind must match **and** an input accepts at most one
  wire (fan-in forbidden); invalid drags are rejected by Svelte Flow.
- **Validation surface** — issues panel bound to a placeholder `issues` array
  (`{severity,node_id?,edge_id?,code,message}`); wire the real server validator into it.
- **Run vs Activate** — Run = simulate/monitor toggle; Activate = deploy
  affordance + revision counter (editing is never live). UI-state only.
- **Signal-flow toggle** drives the edge animation and defaults **off** under
  `prefers-reduced-motion`.
- **Undo/Redo** — snapshot history (`commit()` on connect / node-drag-stop / create), Ctrl+Z / Ctrl+Shift+Z.
- Drag a block from the palette onto the canvas to create a node.

## Performance rules (honoured)

1. Glow is CSS `drop-shadow` on the edge group — never `feGaussianBlur` per edge.
2. Background is its own `<canvas>` GPU layer with its own rAF, decoupled from Svelte state.
3. `prefers-reduced-motion` disables drift + signal flow.
4. Targets fluid behaviour at 100+ nodes (custom node/edge are lightweight; no per-frame Svelte churn).

## Out of scope (later CC tickets)

Real engine/SSE live values, real Activate/Deploy backend, `go:embed`/iframe mount,
server-side persistence. The graph stays the hardcoded demo.

## Notes for the next dev

- Built against **Svelte 5 (runes)** + **`@xyflow/svelte` v1**. If you pin a
  different xyflow major, re-check these API touch-points: `SvelteFlow`
  `bind:nodes/edges/viewport`, `onconnect`, `onnodedragstop`, `addEdge`,
  `getBezierPath`, and the `nodeTypes`/`edgeTypes` records. They are isolated in
  `App.svelte`, `store.svelte.js`, `BlockNode.svelte`, `SignalEdge.svelte`.
- I could not run `npm install`/`dev` in the authoring environment, so treat the
  first `npm run dev` as the smoke test; the data contracts, component structure
  and styling are the load-bearing deliverable.
