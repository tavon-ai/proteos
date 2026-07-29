/**
 * One-shot guard for starting the OIDC flow automatically.
 *
 * Everyone reaching ProteOS already has a live Zitadel session — they came from
 * Orbit, or from a bookmark in the same browser — so a "Sign in" button is a
 * click that achieves nothing. Starting the flow ourselves lets Zitadel bounce
 * them straight back.
 *
 * The danger is the loop. If the round trip completes and /api/me still says
 * unauthenticated — a blocked cookie, a session store outage, a clock skewed
 * far enough that the new cookie is already expired — then redirecting again
 * bounces the browser between here and Zitadel forever, with no page to read
 * the problem on. So the attempt is marked *before* the redirect and only
 * cleared once a session actually resolves.
 *
 * sessionStorage, not localStorage: the guard should last exactly as long as
 * the tab that hit the problem, and a user opening a fresh tab tomorrow
 * deserves a fresh attempt.
 *
 * Ported from chat's `web/src/auth/autoSignIn.ts`. ProteOS signs out over
 * fetch rather than a navigation, so the sign-out case is handled by marking an
 * attempt in the logout mutation (see api/hooks.ts) rather than by a
 * `?signed_out` marker on a redirect.
 */
const ATTEMPT_KEY = 'proteos.auto-signin-attempted';

/**
 * Storage access throws in Safari's private mode and wherever storage is
 * blocked outright. That is the one case we must not auto-redirect in: without
 * somewhere to record the attempt there is no loop protection, and an infinite
 * bounce is far worse than the click it was meant to remove.
 */
function storage(): Storage | null {
  try {
    const s = window.sessionStorage;
    // Touch it: some environments hand back an object that throws on use.
    s.getItem(ATTEMPT_KEY);
    return s;
  } catch {
    return null;
  }
}

/**
 * The client-side counterpart of the server's sanitizeNext: the same rule,
 * applied before the value is ever put in a URL. The server sanitizes again —
 * this exists so a bad value never leaves the browser, not so the server can
 * stop checking.
 *
 * Lives here rather than beside the Login component so that file exports only
 * a component, which is what keeps fast refresh working.
 */
export function safeNext(next: string | null): string {
  if (!next || !next.startsWith('/') || next.startsWith('//') || next.startsWith('/\\')) {
    return '/';
  }
  return next;
}

/** True when we may start the flow without being asked. */
export function canAttemptSignIn(): boolean {
  const s = storage();
  return s !== null && s.getItem(ATTEMPT_KEY) === null;
}

/** Records the attempt. Always called before the redirect, never after. */
export function markSignInAttempted(): void {
  storage()?.setItem(ATTEMPT_KEY, '1');
}

/**
 * Clears the guard once a session resolves, so that a later expiry gets its own
 * silent attempt instead of inheriting this one's.
 */
export function clearSignInAttempt(): void {
  storage()?.removeItem(ATTEMPT_KEY);
}
