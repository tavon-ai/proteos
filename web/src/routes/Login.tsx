import { useSearchParams } from "react-router-dom";
import { loginUrl } from "../api/client";

// Human-readable messages for the error codes the callback can redirect with.
const ERRORS: Record<string, string> = {
  not_invited: "This GitHub account isn't on the invite list yet.",
  bad_state: "Your sign-in link expired. Please try again.",
  missing_state: "Your sign-in session expired. Please try again.",
  github_error: "GitHub declined the sign-in.",
  exchange_failed: "Couldn't complete sign-in with GitHub. Please try again.",
  user_fetch_failed: "Couldn't read your GitHub profile. Please try again.",
  internal: "Something went wrong. Please try again.",
};

export function Login() {
  const [params] = useSearchParams();
  const errorCode = params.get("error");
  const message = errorCode ? (ERRORS[errorCode] ?? "Sign-in failed. Please try again.") : null;

  return (
    <div className="centered">
      <div className="card">
        <h1>ProteOS</h1>
        <p className="muted">Your shape-shifting AI workspace.</p>
        {message && <p className="error" role="alert">{message}</p>}
        <a className="btn" href={loginUrl}>
          Sign in with GitHub
        </a>
      </div>
    </div>
  );
}
