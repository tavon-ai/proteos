import { useState } from 'react';
import type { AccessToken, CreatedToken } from '../api/client';
import { useTokenMutations, useTokens } from '../api/hooks';

// TokensPanel lists the user's personal access tokens and lets them mint a new
// one (the plaintext is shown exactly once, with a copy button) or revoke an
// existing one. The secret is never rendered after creation — the listing shows
// only the non-secret prefix, like ProvidersPanel shows only a "Key set" badge
// (AC1). This is the bootstrap surface for the `proteos` CLI: the user copies a
// token here and sets it as PROTEOS_TOKEN.
export function TokensPanel() {
  const { data: tokens, isLoading, isError } = useTokens();
  const { create } = useTokenMutations();
  const [name, setName] = useState('');
  const [created, setCreated] = useState<CreatedToken | null>(null);

  const onCreate = (e: React.FormEvent) => {
    e.preventDefault();
    const trimmed = name.trim();
    if (!trimmed) return;
    create.mutate(
      { name: trimmed },
      {
        onSuccess: (tok) => {
          setCreated(tok);
          setName('');
        },
      },
    );
  };

  return (
    <section className="tokens-panel">
      <h2>Personal access tokens</h2>
      <p className="muted">
        Mint a token to authenticate the <code>proteos</code> CLI. Set it as the{' '}
        <code>PROTEOS_TOKEN</code> environment variable. The token is shown only once — store it
        now; it cannot be recovered later.
      </p>

      {created && <NewTokenBanner created={created} onDismiss={() => setCreated(null)} />}

      <form className="token-create-form" onSubmit={onCreate}>
        <input
          type="text"
          className="token-name-input"
          placeholder="Token name (e.g. laptop cli)"
          value={name}
          autoComplete="off"
          spellCheck={false}
          onChange={(e) => setName(e.target.value)}
        />
        <button className="btn" type="submit" disabled={!name.trim() || create.isPending}>
          {create.isPending ? 'Creating…' : 'Create token'}
        </button>
      </form>
      {create.isError && <span className="error-inline">Could not create token.</span>}

      {isLoading && <p className="muted">Loading tokens…</p>}
      {isError && <p className="error-banner">Could not load tokens.</p>}
      {tokens && tokens.length === 0 && <p className="muted">No tokens yet.</p>}

      <ul className="token-list">
        {tokens?.map((tok) => (
          <TokenRow key={tok.id} token={tok} />
        ))}
      </ul>
    </section>
  );
}

// NewTokenBanner shows the freshly-minted plaintext once, with a copy button.
function NewTokenBanner({ created, onDismiss }: { created: CreatedToken; onDismiss: () => void }) {
  const [copied, setCopied] = useState(false);
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(created.token);
      setCopied(true);
    } catch {
      // Clipboard blocked; the user can still select the text manually.
    }
  };
  return (
    <div className="token-new-banner">
      <p className="token-new-warning">
        Copy your new token <strong>{created.name}</strong> now — it won’t be shown again.
      </p>
      <div className="token-new-value">
        <code className="token-secret">{created.token}</code>
        <button className="btn" type="button" onClick={copy}>
          {copied ? 'Copied' : 'Copy'}
        </button>
      </div>
      <button className="btn-ghost" type="button" onClick={onDismiss}>
        Done
      </button>
    </div>
  );
}

function TokenRow({ token }: { token: AccessToken }) {
  const { revoke } = useTokenMutations();
  const onRevoke = () => {
    if (!window.confirm(`Revoke token "${token.name || token.prefix}"? This cannot be undone.`))
      return;
    revoke.mutate(token.id);
  };
  return (
    <li className="token-row">
      <div className="token-row-head">
        <span className="token-name">{token.name || '(unnamed)'}</span>
        <code className="token-prefix">{token.prefix}…</code>
      </div>
      <div className="token-row-meta muted">
        <span>Created {formatDate(token.created_at)}</span>
        <span>
          {token.last_used_at ? `Last used ${formatDate(token.last_used_at)}` : 'Never used'}
        </span>
        <span>{token.expires_at ? `Expires ${formatDate(token.expires_at)}` : 'No expiry'}</span>
      </div>
      <div className="token-row-actions">
        <button className="btn-ghost" type="button" onClick={onRevoke} disabled={revoke.isPending}>
          {revoke.isPending ? 'Revoking…' : 'Revoke'}
        </button>
      </div>
    </li>
  );
}

function formatDate(iso: string): string {
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleDateString();
}
