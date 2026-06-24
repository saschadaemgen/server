import { defineConfig } from 'vite';
import { svelte } from '@sveltejs/vite-plugin-svelte';

// base must be /a/designer/ so asset paths resolve when the built dist/
// is served by the carvilon server under /a/designer/ (go:embed, separate CC ticket).
export default defineConfig({
  base: '/a/designer/',
  plugins: [svelte()],
});
