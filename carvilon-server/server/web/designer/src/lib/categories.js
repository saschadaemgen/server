/* Category metadata. `key` matches catalog.json `category` (German, 1:1 with the
   Go descriptor); `label` is the English UI string. Colors are the cyber-neon palette. */
export const CATEGORY = {
  Eingang:  { label: 'Input',  color: '#2DD4EF' },
  Logik:    { label: 'Logic',  color: '#A78BFA' },
  Zeit:     { label: 'Timing', color: '#F6B23C' },
  Speicher: { label: 'Memory', color: '#5B9DFF' },
  Ausgang:  { label: 'Output', color: '#43E08A' },
};

/* Port data-type -> colour/shape. bool = circle/blue, float = diamond/amber, text = square/violet. */
export const KIND = {
  bool:  { color: '#3B82F6', shape: 'circle' },
  float: { color: '#F6B23C', shape: 'diamond' },
  text:  { color: '#A78BFA', shape: 'square' },
};
