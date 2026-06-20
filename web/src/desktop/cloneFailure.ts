// looksLikeGrantFailure reports whether a failed clone's detail points at an
// access problem rather than a network/path error. The guest clones with
// GIT_TERMINAL_PROMPT=0, so a repo the user's token can't reach surfaces as an
// auth fatal ("Authentication failed", "could not read Username … terminal
// prompts disabled") or, since GitHub returns "not found" rather than leak a
// private repo's existence, a "Repository not found" fatal. All of these point
// the user at the same fix — grant the repo to ProteOS (or, if public, fix the
// name) — so we treat them as one case.
export function looksLikeGrantFailure(detail: string): boolean {
  const s = detail.toLowerCase();
  return (
    s.includes('authentication failed') ||
    s.includes('could not read username') ||
    s.includes('terminal prompts disabled') ||
    s.includes('repository not found') ||
    s.includes('not found') ||
    s.includes('403')
  );
}
