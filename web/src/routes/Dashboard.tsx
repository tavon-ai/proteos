import { useNavigate } from "react-router-dom";
import type { Me } from "../api/client";
import { useLogout } from "../api/hooks";

export function Dashboard({ me }: { me: Me }) {
  const navigate = useNavigate();
  const logout = useLogout();

  const onLogout = () => {
    logout.mutate(undefined, {
      // Whether or not the request succeeds we want the user back at /login;
      // the cookie is cleared server-side and the query cache is wiped.
      onSettled: () => navigate("/login", { replace: true }),
    });
  };

  return (
    <div className="app">
      <header className="topbar">
        <span className="brand">ProteOS</span>
        <div className="user">
          {me.user.avatar_url && (
            <img className="avatar" src={me.user.avatar_url} alt="" width={28} height={28} />
          )}
          <span>{me.user.login}</span>
          <button className="btn-ghost" onClick={onLogout} disabled={logout.isPending}>
            {logout.isPending ? "Signing out…" : "Sign out"}
          </button>
        </div>
      </header>

      <main className="content">
        <section className="empty-state">
          <h2>No machine yet</h2>
          <p className="muted">
            You don't have a workspace machine yet. Creating one will spin up an
            isolated environment for your AI coding agents.
          </p>
          <button className="btn" disabled title="Available in a future phase">
            Create machine
          </button>
          <p className="hint">Machine creation isn't enabled yet — coming in Phase 2.</p>
        </section>
      </main>
    </div>
  );
}
