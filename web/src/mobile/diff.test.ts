import { describe, expect, it } from 'vitest';
import { parsePatch } from './diff';

describe('parsePatch', () => {
  it('returns no lines for an absent patch', () => {
    expect(parsePatch('')).toEqual([]);
  });

  it('classifies hunk/add/del/context lines and strips prefixes', () => {
    const patch = [
      '@@ -1,3 +1,4 @@ func main() {',
      ' unchanged',
      '-removed line',
      '+added line',
      '+',
    ].join('\n');
    expect(parsePatch(patch)).toEqual([
      { kind: 'hunk', text: '@@ -1,3 +1,4 @@ func main() {' },
      { kind: 'ctx', text: 'unchanged' },
      { kind: 'del', text: 'removed line' },
      { kind: 'add', text: 'added line' },
      { kind: 'add', text: '' },
    ]);
  });

  it('keeps the no-newline marker as context', () => {
    const lines = parsePatch('@@ -1 +1 @@\n-a\n+b\n\\ No newline at end of file');
    expect(lines[3]).toEqual({ kind: 'ctx', text: '\\ No newline at end of file' });
  });
});
