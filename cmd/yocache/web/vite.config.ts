import { defineConfig } from 'vite';
import { svelte } from '@sveltejs/vite-plugin-svelte';

// Dashboard is served by the Go binary at /ui/ (see cmd/yocache/web.go).
// `base` prefixes every built asset URL with /ui/ so index.html points at
// /ui/assets/… when embedded and served from the Go server.
export default defineConfig({
  plugins: [svelte()],
  base: '/ui/',
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
