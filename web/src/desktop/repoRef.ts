// CloneRef is a normalized clone target for POST /api/git/clone: the display
// label the launcher keys its rows by, plus exactly one of the two request
// shapes — fullName (the GitHub path) or url (clone-by-URL; the control plane
// enforces its host allowlist and answers forbidden_host for the rest).
export interface CloneRef {
  display: string;
  fullName?: string;
  url?: string;
}

// ownerRepoRe mirrors the control plane's validFullName: owner/repo of
// path-safe characters. All-dot segments are rejected separately below.
const ownerRepoRe = /^([A-Za-z0-9_.-]+)\/([A-Za-z0-9_.-]+)$/;

const isDots = (s: string) => /^\.{1,2}$/.test(s);

// parseCloneRef normalizes a repo reference. GitHub forms — bare "owner/repo",
// a github.com URL (https, scp-style, or bare path) — become a fullName ref,
// preserving the pre-multi-host behavior. Any other https://host/owner/repo
// URL becomes a url ref displayed as "host/owner/repo" so same-named repos
// from different hosts stay distinguishable. Returns null when the input fits
// neither shape (the server would reject it anyway).
export function parseCloneRef(input: string): CloneRef | null {
  const github = input
    .trim()
    .replace(/^git@github\.com:/i, '')
    .replace(/^https?:\/\/github\.com\//i, '')
    .replace(/^github\.com\//i, '')
    .replace(/\.git$/i, '')
    .replace(/\/+$/, '');
  const gh = github.match(ownerRepoRe);
  if (gh && !isDots(gh[1]) && !isDots(gh[2])) {
    return { display: `${gh[1]}/${gh[2]}`, fullName: `${gh[1]}/${gh[2]}` };
  }

  const url = input.trim().match(/^https:\/\/([A-Za-z0-9][A-Za-z0-9.-]*(?::\d{1,5})?)\/(.+?)\/*$/);
  if (!url) return null;
  const host = url[1].toLowerCase();
  const path = url[2].replace(/\.git$/i, '');
  const m = path.match(ownerRepoRe);
  if (!m || isDots(m[1]) || isDots(m[2])) return null;
  return {
    display: `${host}/${m[1]}/${m[2]}`,
    url: `https://${host}/${m[1]}/${m[2]}`,
  };
}
