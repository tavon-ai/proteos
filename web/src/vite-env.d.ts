/// <reference types="vite/client" />

// Typed access to the build-time env vars the app reads. VITE_LOG_LEVEL tunes the
// web UI's structured logger (see lib/logger.ts); unset falls back to debug in dev
// and info in production.
interface ImportMetaEnv {
  readonly VITE_LOG_LEVEL?: 'debug' | 'info' | 'warn' | 'error';
}

interface ImportMeta {
  readonly env: ImportMetaEnv;
}
