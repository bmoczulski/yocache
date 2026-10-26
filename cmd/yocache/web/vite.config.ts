import { defineConfig } from 'vite';
import { svelte } from '@sveltejs/vite-plugin-svelte';

// Dashboard is served by the Go binary at /ui/ (see cmd/yocache/web.go).
// A relative `base` makes index.html point at ./assets/… rather than
// /ui/assets/…, so the dashboard also works behind a reverse proxy that
// strips a path prefix (served at /cache1/ui/, forwarded as /ui/).
export default defineConfig({
  plugins: [svelte()],
  base: './',
  build: {
    outDir: 'dist',
    emptyOutDir: true,
  },
  server: {
    proxy: {
      '/api': 'http://localhost:6768',
      '/healthz': 'http://localhost:6768',
      '/version': 'http://localhost:6768',
    },
  },
});
