import { useSearchParams } from 'react-router-dom';
import { githubConnectUrl } from '../api/client';
import { useLogout } from '../api/hooks';

// Human-readable messages for the codes the GitHub connect callback can
// redirect back with (?github_error=...).
const ERRORS: Record<string, string> = {
  not_invited: "This GitHub account isn't on the invite list yet.",
  bad_state: 'Your connect link expired. Please try again.',
  missing_state: 'Your connect session expired. Please try again.',
  missing_code: "GitHub didn't return a code. Please try again.",
  github_error: 'GitHub declined the connection.',
  exchange_failed: "Couldn't complete the GitHub connection. Please try again.",
  user_fetch_failed: "Couldn't read your GitHub profile. Please try again.",
  github_already_linked: 'That GitHub account is already linked to another ProteOS user.',
  internal: 'Something went wrong. Please try again.',
};

// ConnectGitHub is the TAV-149 gate: signing in is Zitadel, but ProteOS needs
// a linked GitHub account for git operations, so the app blocks here until the
// user completes the (one-time) connect flow.
export function ConnectGitHub({ login }: { login: string }) {
  const [params] = useSearchParams();
  const errorCode = params.get('github_error');
  const message = errorCode ? (ERRORS[errorCode] ?? 'Connecting GitHub failed. Please try again.') : null;
  const logout = useLogout();

  return (
    <div className="centered">
      <div className="card">
        <h1>Connect GitHub</h1>
        <p className="muted">
          You're signed in as {login}. ProteOS uses your GitHub account for repositories, commits,
          and pull requests — connect it once to start using your workspace.
        </p>
        {message && (
          <p className="error" role="alert">
            {message}
          </p>
        )}
        <a className="btn" href={githubConnectUrl}>
          Connect GitHub
        </a>
        <p className="muted">
          <button
            type="button"
            className="btn"
            onClick={() => logout.mutate(undefined, { onSuccess: () => window.location.assign('/login') })}
          >
            Sign out
          </button>
        </p>
      </div>
    </div>
  );
}
