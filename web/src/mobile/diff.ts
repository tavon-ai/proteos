// Splits a per-file unified-diff patch (GitHub's `patch` field) into typed
// lines for the mobile diff view. The patch has no file header — it starts
// straight at the first hunk (`@@ … @@`).

export type DiffLineKind = 'hunk' | 'add' | 'del' | 'ctx';

export interface DiffLine {
  kind: DiffLineKind;
  text: string;
}

export function parsePatch(patch: string): DiffLine[] {
  if (!patch) return [];
  return patch.split('\n').map((line): DiffLine => {
    if (line.startsWith('@@')) return { kind: 'hunk', text: line };
    if (line.startsWith('+')) return { kind: 'add', text: line.slice(1) };
    if (line.startsWith('-')) return { kind: 'del', text: line.slice(1) };
    if (line.startsWith('\\')) return { kind: 'ctx', text: line }; // "\ No newline at end of file"
    return { kind: 'ctx', text: line.slice(1) }; // context lines carry a leading space
  });
}
