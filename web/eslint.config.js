import js from '@eslint/js';
import tseslint from 'typescript-eslint';
import reactHooks from 'eslint-plugin-react-hooks';
import reactRefresh from 'eslint-plugin-react-refresh';
import prettier from 'eslint-config-prettier';
import globals from 'globals';

// Flat config for the React + TypeScript web app. typescript-eslint provides the
// TS-aware rules, the react-hooks/react-refresh plugins guard hook usage and
// fast-refresh boundaries, and eslint-config-prettier (last) switches off every
// stylistic rule so Prettier owns formatting and the two never fight.
export default tseslint.config(
  { ignores: ['dist', 'node_modules', '.vite'] },
  {
    files: ['**/*.{ts,tsx}'],
    extends: [js.configs.recommended, ...tseslint.configs.recommended],
    languageOptions: {
      ecmaVersion: 2020,
      globals: { ...globals.browser },
    },
    plugins: {
      'react-hooks': reactHooks,
      'react-refresh': reactRefresh,
    },
    rules: {
      // The two long-standing hook rules. v7's `recommended` also bundles the
      // newer React-Compiler-style rules (set-state-in-effect, refs, globals)
      // which flag a lot of existing working code; opt into just these two so
      // the lint task can gate CI without a codebase rewrite.
      'react-hooks/rules-of-hooks': 'error',
      'react-hooks/exhaustive-deps': 'warn',
      'react-refresh/only-export-components': ['warn', { allowConstantExport: true }],
      '@typescript-eslint/no-unused-vars': ['error', { argsIgnorePattern: '^_' }],
    },
  },
  // Test files run under Vitest globals (describe/it/expect) and the jsdom env.
  {
    files: ['**/*.test.{ts,tsx}'],
    languageOptions: {
      globals: { ...globals.node },
    },
  },
  prettier,
);
