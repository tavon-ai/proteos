import { loginUrl } from "../api/client";

// GitHubStatus renders the GitHub connection state. When the grant is healthy it
// shows a small "connected" chip; when the control plane reports
// reconnect_github (revoked grant / dead refresh token) it shows an actionable
// Reconnect banner that re-runs the login flow (Phase 7 decision #8). The
// distinction is the one place GitHub-App auth is user-visibly different from a
// classic OAuth token, so surfacing it beats "why did my git stop working?".
export function GitHubStatus({ reconnect }: { reconnect: boolean }) {
  if (reconnect) {
    return (
      <div className="github-reconnect" role="alert">
        <span>
          Your GitHub connection has expired or was revoked. Reconnect to keep
          cloning and pushing.
        </span>
        {/* Full navigation (not fetch) so the browser follows GitHub's 302 and
            re-links the grant, clearing the revoked flag server-side. */}
        <a className="btn-primary" href={loginUrl}>
          Reconnect GitHub
        </a>
      </div>
    );
  }
  return (
    <span className="github-chip" title="GitHub connected">
      <span className="dot dot-ok" aria-hidden="true" /> GitHub connected
    </span>
  );
}
