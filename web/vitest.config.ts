import { defineConfig } from 'vitest/config';
import react from '@vitejs/plugin-react';

// Vitest config for the desktop unit tests. The window-manager core is pure and
// runs in the node environment; the few component tests opt into jsdom per-file
// via the `// @vitest-environment jsdom` pragma.
export default defineConfig({
  plugins: [react()],
  test: {
    environment: 'node',
    include: ['src/**/*.test.{ts,tsx}'],
  },
});
