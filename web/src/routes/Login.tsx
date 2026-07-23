import { useSearchParams } from 'react-router-dom';
import { loginUrl } from '../api/client';

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
        <a className="btn" href={loginUrl}>
          Sign in
        </a>
      </div>
    </div>
  );
}
