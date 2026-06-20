// parseRepoRef normalizes a GitHub repo reference to "owner/repo". It accepts a
// bare "owner/repo", an https URL, an scp-style git URL, or a "github.com/…"
// path, tolerating a trailing ".git" or slash. Returns null when the input
// doesn't look like a GitHub owner/repo, mirroring the control plane's
// fullNameRe so the clone POST won't be rejected with bad_full_name.
export function parseRepoRef(input: string): string | null {
  const stripped = input
    .trim()
    .replace(/^git@github\.com:/i, '')
    .replace(/^https?:\/\/github\.com\//i, '')
    .replace(/^github\.com\//i, '')
    .replace(/\.git$/i, '')
    .replace(/\/+$/, '');
  const m = stripped.match(/^([A-Za-z0-9_.-]+)\/([A-Za-z0-9_.-]+)$/);
  return m ? `${m[1]}/${m[2]}` : null;
}
