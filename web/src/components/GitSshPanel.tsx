import { useEffect, useState } from 'react';
import {
  useGitIdentity,
  useGitIdentityMutations,
  useSSHKey,
  useSSHKeyMutations,
} from '../api/hooks';

// GitSshPanel manages the two most-wanted portable files (Phase 4): the git
// identity rendered to ~/.gitconfig, and an SSH key materialized 0600 under
// ~/.ssh. The private key is never shown — only the public key, for the user to
// add to GitHub. Both follow the user onto every machine they create.
export function GitSshPanel() {
  return (
    <section className="providers-panel">
      <h2>Git &amp; SSH</h2>
      <p className="muted">
        Your git identity and SSH key follow you onto every machine you create, so you can commit
        with the right name and push over SSH without per-machine setup.
      </p>
      <GitIdentitySection />
      <hr className="panel-divider" />
      <SshKeySection />
    </section>
  );
}

function GitIdentitySection() {
  const { data, isLoading } = useGitIdentity();
  const { set, clear } = useGitIdentityMutations();
  const [editing, setEditing] = useState(false);
  const [name, setName] = useState('');
  const [email, setEmail] = useState('');

  // Seed the form from the effective identity when entering edit mode.
  useEffect(() => {
    if (editing && data) {
      setName(data.name);
      setEmail(data.email);
    }
  }, [editing, data]);

  const onSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    if (!name.trim() || !email.trim()) return;
    set.mutate({ name: name.trim(), email: email.trim() }, { onSuccess: () => setEditing(false) });
  };

  return (
    <div className="provider-row">
      <div className="provider-row-head">
        <span className="provider-name">Git identity</span>
        {data && (
          <span
            className={`badge ${data.source === 'profile' ? 'badge-running' : 'badge-stopped'}`}
          >
            {data.source === 'profile' ? 'Custom' : 'From GitHub'}
          </span>
        )}
      </div>

      {isLoading && <p className="muted">Loading…</p>}
      {data && !editing && (
        <>
          <p className="muted">
            <code>{data.name}</code> &lt;{data.email}&gt;
            {data.source === 'github' && ' — defaulting to your GitHub identity.'}
          </p>
          <div className="provider-row-actions">
            <button className="btn" onClick={() => setEditing(true)}>
              {data.source === 'profile' ? 'Edit identity' : 'Set custom identity'}
            </button>
            {data.source === 'profile' && (
              <button
                className="btn-ghost"
                onClick={() => clear.mutate()}
                disabled={clear.isPending}
              >
                {clear.isPending ? 'Resetting…' : 'Reset to GitHub'}
              </button>
            )}
          </div>
        </>
      )}

      {editing && (
        <form className="provider-key-form" onSubmit={onSubmit}>
          <label className="provider-field">
            <span className="provider-field-label">Name</span>
            <input
              className="provider-key-input"
              value={name}
              autoFocus
              onChange={(e) => setName(e.target.value)}
            />
          </label>
          <label className="provider-field">
            <span className="provider-field-label">Email</span>
            <input
              className="provider-key-input"
              type="email"
              value={email}
              onChange={(e) => setEmail(e.target.value)}
            />
          </label>
          <div className="provider-row-actions">
            <button
              className="btn"
              type="submit"
              disabled={!name.trim() || !email.trim() || set.isPending}
            >
              {set.isPending ? 'Saving…' : 'Save'}
            </button>
            <button className="btn-ghost" type="button" onClick={() => setEditing(false)}>
              Cancel
            </button>
          </div>
          {set.isError && <span className="error-inline">Could not save identity.</span>}
        </form>
      )}
    </div>
  );
}

function SshKeySection() {
  const { data, isLoading } = useSSHKey();
  const { generate, remove } = useSSHKeyMutations();
  // The public key shown right after generation (also available from status).
  const [copied, setCopied] = useState(false);
  const publicKey = generate.data?.public_key ?? data?.public_key;
  const fingerprint = generate.data?.fingerprint ?? data?.fingerprint;
  const present = data?.present || !!generate.data?.present;

  const onGenerate = () => {
    if (present && !window.confirm('Replace your existing SSH key? The old key stops working.')) {
      return;
    }
    generate.mutate();
  };

  const onRemove = () => {
    if (
      !window.confirm(
        'Remove your SSH key? It stops being injected into your machines. The public key is NOT ' +
          'removed from GitHub — delete it there too if you no longer want it accepted.',
      )
    ) {
      return;
    }
    remove.mutate();
  };

  const onCopy = () => {
    if (!publicKey) return;
    void navigator.clipboard?.writeText(publicKey);
    setCopied(true);
    setTimeout(() => setCopied(false), 1500);
  };

  return (
    <div className="provider-row">
      <div className="provider-row-head">
        <span className="provider-name">SSH key</span>
        <span className={`badge ${present ? 'badge-running' : 'badge-stopped'}`}>
          {present ? 'Configured' : 'None'}
        </span>
      </div>

      {present && publicKey && (
        <>
          <p className="muted">
            Add this public key to{' '}
            <a href="https://github.com/settings/keys" target="_blank" rel="noreferrer">
              GitHub → SSH keys ↗
            </a>{' '}
            so pushes over SSH work. The private key stays on the server and is injected into your
            machines — it is never shown.
          </p>
          <textarea className="ssh-public-key" readOnly rows={2} value={publicKey} />
          {fingerprint && <p className="muted">Fingerprint: {fingerprint}</p>}
          <div className="provider-row-actions">
            <button className="btn" onClick={onCopy}>
              {copied ? 'Copied' : 'Copy public key'}
            </button>
            <button className="btn-ghost" onClick={onGenerate} disabled={generate.isPending}>
              {generate.isPending ? 'Regenerating…' : 'Regenerate'}
            </button>
            <button className="btn-ghost" onClick={onRemove} disabled={remove.isPending}>
              {remove.isPending ? 'Removing…' : 'Remove'}
            </button>
          </div>
        </>
      )}

      {/* Show the empty state whenever there's no key — even while loading, so the
          section never appears blank. The button is disabled until loading completes. */}
      {!present && (
        <>
          <p className="muted">
            Generate an SSH key that follows you onto every machine. The private key is stored
            encrypted and injected at runtime; you add the public key to GitHub.
          </p>
          <div className="provider-row-actions">
            <button className="btn" onClick={onGenerate} disabled={isLoading || generate.isPending}>
              {generate.isPending ? 'Generating…' : 'Generate SSH key'}
            </button>
          </div>
        </>
      )}

      {/* Fallback: key is marked present but no public key came back (edge case). */}
      {present && !publicKey && !isLoading && (
        <div className="provider-row-actions">
          <button className="btn-ghost" onClick={onGenerate} disabled={generate.isPending}>
            {generate.isPending ? 'Regenerating…' : 'Regenerate key'}
          </button>
        </div>
      )}

      {generate.isError && <span className="error-inline">Could not generate key.</span>}
    </div>
  );
}
