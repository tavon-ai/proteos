import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

// The dev server proxies /api to the Go control plane so the SPA and API share
// an origin in development (cookies + CSRF header behave as in production).
export default defineConfig({
  plugins: [react()],
  // react-rnd's bundled react-draggable reads process.env.DRAGGABLE_DEBUG on
  // every drag start; the browser has no `process`, so without this define any
  // window-header mousedown throws ReferenceError and unmounts the app
  // (react-draggable issue #926). Defined for both the dev dep optimizer
  // (esbuild) and the production build (rollup).
  define: { 'process.env.DRAGGABLE_DEBUG': 'false' },
  optimizeDeps: {
    esbuildOptions: { define: { 'process.env.DRAGGABLE_DEBUG': 'false' } },
  },
  server: {
    port: 5173,
    proxy: {
      '/api': {
        target: 'http://localhost:8080',
        changeOrigin: false,
      },
      // The terminal gateway WebSocket. ws:true upgrades the proxied connection;
      // changeOrigin:false preserves the browser Origin (http://localhost:5173)
      // so the gateway's Origin allowlist accepts it.
      '/gw': {
        target: 'http://localhost:8080',
        changeOrigin: false,
        ws: true,
      },
    },
  },
});
