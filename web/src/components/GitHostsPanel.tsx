import { useState } from 'react';
import { ApiError } from '../api/client';
import type { GitHost } from '../api/client';
import { useGitHostMutations, useGitHosts } from '../api/hooks';

// GitHostsPanel lists the operator-allowlisted additional git hosts (Gitea/
// Forgejo) and lets the user save (write-only), replace, or remove a Personal
// Access Token per host. The token is validated against the host on save —
// which also captures the account login shown next to the host — and is never
// rendered back, mirroring the ProvidersPanel key UX. Renders nothing when the
// operator has configured no extra hosts.
export function GitHostsPanel() {
  const { data: hosts, isLoading, isError } = useGitHosts();

  if (!isLoading && !isError && (hosts?.length ?? 0) === 0) return null;

  return (
    <section className="githosts-panel">
      <h2>Git hosts</h2>
      <p className="muted">
        Public repos on these hosts clone without a token. Save a Personal Access Token to also
        push, open pull requests, and use private repos there. The token is checked against the
        host, stored encrypted, and never shown again.
      </p>

      {isLoading && <p className="muted">Loading git hosts…</p>}
      {isError && <p className="error-banner">Could not load git hosts.</p>}

      <ul className="provider-list">
        {hosts?.map((h) => (
          <GitHostRow key={h.host} host={h} />
        ))}
      </ul>
    </section>
  );
}

function GitHostRow({ host }: { host: GitHost }) {
  const { setToken, deleteToken } = useGitHostMutations();
  const [editing, setEditing] = useState(false);
  const [value, setValue] = useState('');

  const reset = () => {
    setValue('');
    setEditing(false);
  };

  const onSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    if (!value.trim()) return;
    setToken.mutate({ host: host.host, token: value.trim() }, { onSuccess: reset });
  };

  const onDelete = () => {
    if (!window.confirm(`Remove your token for ${host.host}?`)) return;
    deleteToken.mutate(host.host);
  };

  // A rejected token is the user's to fix; anything else is a server/host issue.
  const saveError =
    setToken.error instanceof ApiError && setToken.error.code === 'bad_token'
      ? 'The host rejected this token — check it and try again.'
      : 'Could not save the token.';

  return (
    <li className="provider-row">
      <div className="provider-row-head">
        <span className="provider-name">{host.host}</span>
        <span className={`badge ${host.linked ? 'badge-running' : 'badge-stopped'}`}>
          {host.linked ? `Token saved${host.login ? ` · ${host.login}` : ''}` : 'No token'}
        </span>
      </div>

      {editing ? (
        <form className="provider-key-form" onSubmit={onSubmit}>
          <label className="provider-field">
            <span className="provider-field-label">Personal Access Token</span>
            <input
              type="password"
              className="provider-key-input"
              placeholder="Personal Access Token"
              value={value}
              autoFocus
              autoComplete="off"
              spellCheck={false}
              onChange={(e) => setValue(e.target.value)}
            />
          </label>
          <div className="provider-row-actions">
            <button className="btn" type="submit" disabled={!value.trim() || setToken.isPending}>
              {setToken.isPending ? 'Checking…' : 'Save'}
            </button>
            <button className="btn-ghost" type="button" onClick={reset}>
              Cancel
            </button>
          </div>
          {setToken.isError && <span className="error-inline">{saveError}</span>}
        </form>
      ) : (
        <div className="provider-row-actions">
          <button className="btn" onClick={() => setEditing(true)}>
            {host.linked ? 'Replace token' : 'Add token'}
          </button>
          {host.linked && (
            <button className="btn-ghost" onClick={onDelete} disabled={deleteToken.isPending}>
              {deleteToken.isPending ? 'Removing…' : 'Remove'}
            </button>
          )}
        </div>
      )}
    </li>
  );
}
