// One-time vendoring of the cockpit shell's third-party ESM libraries.
//
// Produces ONE fully self-contained ESM bundle, public/vendor/lib.js, so the
// cockpit runs offline with no build step, no CDN and no import map. React is
// bundled exactly once; React-DOM and React Flow reference that same internal
// instance, so there is a single React instance (no "invalid hook call") and
// no runtime `require()` shim (the trap when react is an external CJS dep).
//
//   npm ci && npm run vendor
//
// The produced files are committed; running the cockpit never needs this.
import { build } from "esbuild";
import { mkdirSync, copyFileSync, rmSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const root = resolve(here, ".."); // tools/cockpit (where node_modules lives)
const outDir = resolve(root, "public", "vendor");
mkdirSync(outDir, { recursive: true });

// drop any earlier split bundles
for (const stale of ["react.js", "react-dom.js", "react-dom-client.js", "xyflow-react.js", "dagre.js", "marked.js", "htm.js"]) {
  rmSync(resolve(outDir, stale), { force: true });
}

const entry = `
import React from "react";
import { createRoot } from "react-dom/client";
import * as XYFlow from "@xyflow/react";
import dagre from "@dagrejs/dagre";
import { marked, Marked } from "marked";
import htm from "htm";
export { React, createRoot, XYFlow, dagre, marked, Marked, htm };
`;

await build({
  bundle: true,
  format: "esm",
  minify: true,
  legalComments: "none",
  target: "es2020",
  logLevel: "info",
  define: { "process.env.NODE_ENV": '"production"' },
  stdin: { contents: entry, resolveDir: root, loader: "js" },
  outfile: resolve(outDir, "lib.js"),
});

copyFileSync(resolve(root, "node_modules/@xyflow/react/dist/style.css"), resolve(outDir, "xyflow.css"));

console.log("\nvendored -> public/vendor/lib.js (+ xyflow.css)");
