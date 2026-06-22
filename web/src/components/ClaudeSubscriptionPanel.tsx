import { useState } from 'react';
import { CLAUDE_OAUTH_KEY, type ProfileItem } from '../api/client';
import { useProfileItems, useProfileMutations } from '../api/hooks';

// ClaudeSubscriptionPanel lets a user connect their Claude Pro/Max/Team/Enterprise
// subscription to ProteOS by pasting a `claude setup-token` token once. The token
// is then materialized into every machine the user creates (and re-injected into
// already-running machines on connect/disconnect), so `claude` launches
// authenticated with no per-machine login flow. The token is write-only: it is
// never rendered back — the panel shows only connected / reconnect-needed status
// derived from the server's metadata, mirroring the providers panel.
export function ClaudeSubscriptionPanel() {
  const { data: items, isLoading, isError } = useProfileItems();
  const claude = items?.find((i) => i.key === CLAUDE_OAUTH_KEY);

  return (
    <section className="providers-panel">
      <h2>Claude subscription</h2>
      <p className="muted">
        Connect your Claude subscription once and every machine you create launches with{' '}
        <code>claude</code> already signed in — no per-machine login. Your token is stored encrypted
        and injected at runtime; it is never shown again.
      </p>

      {isLoading && <p className="muted">Loading…</p>}
      {isError && <p className="error-banner">Could not load subscription status.</p>}

      {!isLoading && !isError && <ClaudeSubscriptionRow item={claude} />}
    </section>
  );
}

function statusBadge(item: ProfileItem | undefined) {
  if (!item) return { className: 'badge badge-stopped', label: 'Not connected' };
  if (item.needs_reconnect) return { className: 'badge badge-error', label: 'Reconnect needed' };
  return { className: 'badge badge-running', label: 'Connected' };
}

function ClaudeSubscriptionRow({ item }: { item: ProfileItem | undefined }) {
  const { setItem, deleteItem } = useProfileMutations();
  const [editing, setEditing] = useState(false);
  const [value, setValue] = useState('');

  const connected = !!item;
  const badge = statusBadge(item);

  const reset = () => {
    setValue('');
    setEditing(false);
  };

  const onSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    const token = value.trim();
    if (!token) return;
    setItem.mutate({ key: CLAUDE_OAUTH_KEY, value: token }, { onSuccess: reset });
  };

  const onDisconnect = () => {
    if (
      !window.confirm(
        'Disconnect your Claude subscription? This stops the token from being injected into ' +
          'your machines. It does NOT revoke the token with Anthropic — to fully revoke it, ' +
          'remove it from your Claude account.',
      )
    ) {
      return;
    }
    deleteItem.mutate(CLAUDE_OAUTH_KEY);
  };

  return (
    <div className="provider-row">
      <div className="provider-row-head">
        <span className="provider-name">Claude Code subscription</span>
        <span className={badge.className}>{badge.label}</span>
      </div>

      {item?.needs_reconnect && (
        <p className="error-inline">
          Your stored token has expired. Generate a new one and reconnect to keep machines signed
          in.
        </p>
      )}

      {editing ? (
        <form className="provider-key-form" onSubmit={onSubmit}>
          <p className="muted">
            Generate a token by running <code>claude setup-token</code> on a machine where you are
            signed in, then paste the result below. The token is valid for about a year.
          </p>
          <label className="provider-field">
            <span className="provider-field-label">Subscription token</span>
            <input
              type="password"
              className="provider-key-input"
              placeholder="claude setup-token output"
              value={value}
              autoFocus
              autoComplete="off"
              spellCheck={false}
              onChange={(e) => setValue(e.target.value)}
            />
          </label>
          <div className="provider-row-actions">
            <button className="btn" type="submit" disabled={!value.trim() || setItem.isPending}>
              {setItem.isPending ? 'Saving…' : 'Connect'}
            </button>
            <button className="btn-ghost" type="button" onClick={reset}>
              Cancel
            </button>
          </div>
          {setItem.isError && <span className="error-inline">Could not save token.</span>}
        </form>
      ) : (
        <div className="provider-row-actions">
          <button className="btn" onClick={() => setEditing(true)}>
            {connected ? 'Replace token' : 'Connect subscription'}
          </button>
          {connected && (
            <button className="btn-ghost" onClick={onDisconnect} disabled={deleteItem.isPending}>
              {deleteItem.isPending ? 'Disconnecting…' : 'Disconnect'}
            </button>
          )}
        </div>
      )}

      {connected && !editing && (
        <p className="muted">
          Disconnecting stops propagation to your machines; it does not revoke the token with
          Anthropic.
        </p>
      )}
    </div>
  );
}
