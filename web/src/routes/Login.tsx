import { useEffect, useState } from 'react';
import { useSearchParams } from 'react-router';
import { loginUrl } from '../api/client';
import { canAttemptSignIn, markSignInAttempted, safeNext } from '../autoSignIn';

// Human-readable messages for the error codes the OIDC callback can redirect
// with (TAV-149: sign-in is Zitadel).
const ERRORS: Record<string, string> = {
  bad_state: 'Your sign-in link expired. Please try again.',
  missing_state: 'Your sign-in session expired. Please try again.',
  missing_code: "The sign-in service didn't return a code. Please try again.",
  idp_error: 'The sign-in service declined the sign-in.',
  idp_unreachable: "Couldn't reach the sign-in service. Please try again.",
  exchange_failed: "Couldn't complete sign-in. Please try again.",
  user_fetch_failed: "Couldn't read your profile. Please try again.",
  link_ambiguous: 'Your email matches more than one existing account. Contact an administrator.',
  internal: 'Something went wrong. Please try again.',
};

export function Login() {
  const [params] = useSearchParams();
  const errorCode = params.get('error');
  const message = errorCode ? (ERRORS[errorCode] ?? 'Sign-in failed. Please try again.') : null;
  const next = safeNext(params.get('next'));
  const href = next === '/' ? loginUrl : `${loginUrl}?next=${encodeURIComponent(next)}`;

  // An error code means the flow already ran and came back broken. Never
  // auto-retry it: that is the loop this page exists to break, and the message
  // above is the only place the user can read what went wrong.
  //
  // Read once on mount, deliberately: marking the attempt would otherwise flip
  // this mid-flight and flash the card in the gap between the mark and the
  // navigation.
  const [signingIn] = useState(() => !errorCode && canAttemptSignIn());

  useEffect(() => {
    if (!signingIn) return;
    markSignInAttempted();
    window.location.assign(href);
  }, [signingIn, href]);

  if (signingIn) {
    return (
      <div className="centered">
        <div className="card">
          <h1>ProteOS</h1>
          <p className="muted">Signing you in…</p>
        </div>
      </div>
    );
  }

  // The fallback surface, reached when the automatic attempt came back still
  // signed out, when an error code says it failed, or when there was nowhere to
  // record an attempt. Keeping it is what stops a misconfiguration turning into
  // a bounce with nowhere to read the error.
  return (
    <div className="centered">
      <div className="card">
        <h1>ProteOS</h1>
        <p className="muted">Your shape-shifting AI workspace.</p>
        {message && (
          <p className="error" role="alert">
            {message}
          </p>
        )}
        <a className="btn" href={href}>
          Sign in
        </a>
      </div>
    </div>
  );
}
