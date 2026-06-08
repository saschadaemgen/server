// Shared helpers. No build step: htm gives JSX-like ergonomics over
// React.createElement, bound to the single vendored React instance.
import { React, htm } from "/vendor/lib.js";

export const html = htm.bind(React.createElement);
export const { useState, useEffect, useMemo, useRef, useCallback, Fragment } = React;

export function cx(...xs) {
  return xs.filter(Boolean).join(" ");
}

export async function fetchText(url) {
  const r = await fetch(url);
  if (!r.ok) throw new Error(`${r.status} ${r.statusText}`);
  return r.text();
}

export async function fetchJSON(url) {
  const r = await fetch(url);
  if (!r.ok) throw new Error(`${r.status} ${r.statusText}`);
  return r.json();
}

// Minimal inline-SVG icon set (generic, crisp at any size).
const PATHS = {
  bauplan: "M3 3h7v7H3zM14 3h7v7h-7zM14 14h7v7h-7zM3 14h7v7H3z",
  doku: "M6 2h9l5 5v15H6zM15 2v5h5",
  planung: "M4 5h16M4 12h16M4 19h16",
  workflow: "M6 3v6M6 21v-6M18 3v12M6 9a3 3 0 0 0 3 3h6a3 3 0 0 1 3 3",
  archiv: "M3 4h18v4H3zM5 8v12h14V8M9 12h6",
  play: "M7 5v14l11-7z",
  pause: "M7 5h4v14H7zM13 5h4v14h-4z",
  prev: "M18 5v14L8 12zM6 5h2v14H6z",
  next: "M6 5v14l10-7zM16 5h2v14h-2z",
  reset: "M3 12a9 9 0 1 0 3-6.7M3 4v4h4",
  dot: "M12 12m-4 0a4 4 0 1 0 8 0a4 4 0 1 0-8 0",
};

export function Icon({ name, size = 18 }) {
  const d = PATHS[name] || PATHS.dot;
  return html`<svg
    class="icon"
    width=${size}
    height=${size}
    viewBox="0 0 24 24"
    fill="none"
    stroke="currentColor"
    stroke-width="2"
    stroke-linecap="round"
    stroke-linejoin="round"
    aria-hidden="true"
  ><path d=${d} /></svg>`;
}
