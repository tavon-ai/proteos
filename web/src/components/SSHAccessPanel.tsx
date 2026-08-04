import { useState } from 'react';
import { ApiError } from '../api/client';
import type { SSHLoginKey } from '../api/client';
import { useMe, useSSHLoginKeys, useSSHLoginKeyMutations, useUpdateUserPrefs } from '../api/hooks';

// SSHAccessPanel is the settings surface for inbound SSH: the account-wide
// on/off switch, the list of public keys authorized to log in, and the connect
// instructions.
//
// Two things about the model are worth stating plainly in the UI, because they
// are what make SSH here different from a normal server:
//  1. A machine has no inbound port and no address of its own. The guest's sshd
//     binds loopback only, and the only path to it is the authenticated
//     control-plane tunnel — so every client needs a ProxyCommand.
//  2. The switch is a real kill-switch, not a display state: with it off the
//     control plane stops injecting ~/.ssh/authorized_keys into the user's
//     machines AND refuses the tunnel.
export function SSHAccessPanel() {
  return (
    <section className="providers-panel">
      <h2>SSH access</h2>
      <p className="muted">
        Log into your machines from a local terminal with <code>ssh</code>, and use <code>scp</code>
        , <code>sftp</code>, <code>rsync</code>, or an IDE&apos;s Remote-SSH over the same
        connection.
      </p>
      <SshSwitchSection />
      <hr className="panel-divider" />
      <SshLoginKeysSection />
      <hr className="panel-divider" />
      <SshInstructionsSection />
    </section>
  );
}

// SshSwitchSection owns the enable/disable checkbox. Disabling is destructive in
// the useful sense — it drops authorized_keys from every machine — so the
// confirm spells that out rather than asking a bare "are you sure?".
function SshSwitchSection() {
  const { data, isLoading } = useMe();
  const update = useUpdateUserPrefs();
  const enabled = data?.prefs.ssh_enabled ?? false;

  const toggle = (next: boolean) => {
    if (update.isPending) return;
    if (
      !next &&
      !window.confirm(
        'Turn off SSH access? Your registered keys stay saved, but they are removed from every ' +
          'machine and new SSH connections are refused until you turn it back on.',
      )
    ) {
      return;
    }
    update.mutate({ ssh_enabled: next });
  };

  return (
    <div className="provider-row">
      <div className="provider-row-head">
        <span className="provider-name">SSH access</span>
        <span className={`badge ${enabled ? 'badge-running' : 'badge-stopped'}`}>
          {enabled ? 'Enabled' : 'Disabled'}
        </span>
      </div>

      <label className="ssh-toggle">
        <input
          type="checkbox"
          checked={enabled}
          disabled={isLoading || update.isPending}
          onChange={(e) => toggle(e.target.checked)}
        />
        <span className="ssh-toggle-text">
          <strong>Allow SSH into my machines</strong>
          <span className="muted">
            {' '}
            — when off, your keys are removed from every machine and connections are refused. Off by
            default: registering a key does not on its own open the door.
          </span>
        </span>
      </label>

      {update.isError && <span className="error-inline">Could not change the setting.</span>}
    </div>
  );
}

// SshLoginKeysSection manages the list of authorized public keys. Note the
// deliberate absence of any "generate a key for me" action: a login key's
// private half must never reach the server, so the user pastes a public key they
// already hold (contrast the Git & SSH panel's outbound key, which IS
// server-generated because the guest is the one using it).
function SshLoginKeysSection() {
  const { data: keys, isLoading } = useSSHLoginKeys();
  const { add, revoke } = useSSHLoginKeyMutations();
  const [adding, setAdding] = useState(false);
  const [label, setLabel] = useState('');
  const [publicKey, setPublicKey] = useState('');

  const onSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    if (!label.trim() || !publicKey.trim()) return;
    add.mutate(
      { label: label.trim(), publicKey: publicKey.trim() },
      {
        onSuccess: () => {
          setLabel('');
          setPublicKey('');
          setAdding(false);
        },
      },
    );
  };

  const onRevoke = (k: SSHLoginKey) => {
    if (
      !window.confirm(
        `Remove "${k.label}"? It stops being accepted on your machines within moments — sshd ` +
          're-reads the key file on every connection.',
      )
    ) {
      return;
    }
    revoke.mutate(k.id);
  };

  const addError =
    add.error instanceof ApiError && add.error.code === 'invalid_key'
      ? "That doesn't look like an SSH public key. Paste one line, starting with something like ssh-ed25519 or ssh-rsa."
      : add.error instanceof ApiError && add.error.code === 'duplicate_key'
        ? 'You have already added this key.'
        : 'Could not add the key.';

  return (
    <div className="provider-row">
      <div className="provider-row-head">
        <span className="provider-name">Authorized keys</span>
        {keys && (
          <span className={`badge ${keys.length > 0 ? 'badge-running' : 'badge-stopped'}`}>
            {keys.length === 0 ? 'None' : `${keys.length} key${keys.length === 1 ? '' : 's'}`}
          </span>
        )}
      </div>

      <p className="muted">
        Public keys allowed to log in. They apply to every machine you own, including ones you
        create later. Paste a <strong>public</strong> key (the <code>.pub</code> file) — your
        private key must never leave this computer, and there is nowhere here to put one.
      </p>

      {isLoading && <p className="muted">Loading…</p>}

      {keys && keys.length > 0 && (
        <ul className="ssh-key-list">
          {keys.map((k) => (
            <li key={k.id} className="ssh-key-row">
              <span className="ssh-key-meta">
                <strong>{k.label}</strong>
                <code className="mono ssh-key-fingerprint">{k.fingerprint}</code>
              </span>
              <button
                type="button"
                className="btn-ghost"
                onClick={() => onRevoke(k)}
                disabled={revoke.isPending}
              >
                Remove
              </button>
            </li>
          ))}
        </ul>
      )}

      {keys && keys.length === 0 && !adding && (
        <p className="muted">
          No keys yet. On most systems your public key is at <code>~/.ssh/id_ed25519.pub</code> —
          print it with <code>cat ~/.ssh/id_ed25519.pub</code>, or create one with{' '}
          <code>ssh-keygen -t ed25519</code>.
        </p>
      )}

      {adding ? (
        <form className="provider-key-form" onSubmit={onSubmit}>
          <label className="provider-field">
            <span className="provider-field-label">Label</span>
            <input
              className="provider-key-input"
              placeholder="Work laptop"
              value={label}
              autoFocus
              onChange={(e) => setLabel(e.target.value)}
            />
          </label>
          <label className="provider-field">
            <span className="provider-field-label">Public key</span>
            <textarea
              className="ssh-public-key"
              rows={3}
              placeholder="ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA... you@laptop"
              value={publicKey}
              onChange={(e) => setPublicKey(e.target.value)}
            />
          </label>
          <div className="provider-row-actions">
            <button
              className="btn-primary"
              type="submit"
              disabled={!label.trim() || !publicKey.trim() || add.isPending}
            >
              {add.isPending ? 'Adding…' : 'Add key'}
            </button>
            <button
              className="btn-ghost"
              type="button"
              onClick={() => {
                setAdding(false);
                add.reset();
              }}
            >
              Cancel
            </button>
          </div>
          {add.isError && <span className="error-inline">{addError}</span>}
        </form>
      ) : (
        <div className="provider-row-actions">
          <button className="btn" type="button" onClick={() => setAdding(true)}>
            Add a key
          </button>
        </div>
      )}
    </div>
  );
}

// SshInstructionsSection is the "how do I actually connect" half. It uses a real
// machine id from the user's own fleet when there is one, so the commands are
// copy-and-run rather than a template to fill in.
function SshInstructionsSection() {
  const { data } = useMe();
  const enabled = data?.prefs.ssh_enabled ?? false;
  const machines = data?.machines ?? [];
  const target = machines.find((m) => m.state === 'running') ?? machines[0];
  const id = target?.id ?? '<machine-id>';
  const name = target?.name;

  return (
    <div className="provider-row">
      <div className="provider-row-head">
        <span className="provider-name">How to connect</span>
      </div>

      {!enabled && (
        <p className="ssh-notice">
          SSH is currently off for your account — turn on{' '}
          <strong>Allow SSH into my machines</strong> above before these will work.
        </p>
      )}

      <p className="muted">
        Machines have no public address and no inbound port: the SSH server inside a machine listens
        only on its own loopback, and the one route to it is an authenticated tunnel through
        ProteOS. So connections go through the <code>proteos</code> CLI, which is also what
        authenticates you. Install it from <strong>Settings → Downloads</strong> and sign in with{' '}
        <code>proteos auth login</code>.
      </p>

      <ol className="ssh-steps">
        <li>
          <span className="ssh-step-label">Connect to a machine</span>
          <CopyBlock text={`proteos ssh ${id}`} />
          {name && (
            <p className="muted">
              That is your machine <strong>{name}</strong>. Use <code>proteos machines ls</code> to
              see the others.
            </p>
          )}
        </li>
        <li>
          <span className="ssh-step-label">Run a single command instead of a shell</span>
          <CopyBlock text={`proteos ssh ${id} -- uname -a`} />
        </li>
        <li>
          <span className="ssh-step-label">
            Use plain ssh, scp, rsync, or an IDE&apos;s Remote-SSH
          </span>
          <p className="muted">
            Add this to <code>~/.ssh/config</code>, then any SSH-based tool can use the host name
            directly — <code>ssh proteos-{id}</code>, <code>scp file proteos-{id}:</code>, or VS
            Code Remote-SSH.
          </p>
          <CopyBlock
            text={`Host proteos-${id}\n    User dev\n    ProxyCommand proteos ssh-proxy ${id}`}
          />
        </li>
      </ol>

      <p className="muted">
        You log in as <code>dev</code>. Password login is disabled entirely — only the keys above
        are accepted. Each machine keeps its own host key across stop/start, so your{' '}
        <code>known_hosts</code> entry stays valid.
      </p>
    </div>
  );
}

// MachineSshPanel is the per-machine counterpart to the settings panel: the
// exact command for THIS machine, shown where someone is already looking at it.
// It intentionally stays small and defers everything configurable (the switch,
// the key list) to Settings → SSH access, since both are account-wide.
export function MachineSshPanel({
  machineId,
  machineState,
}: {
  machineId: string;
  machineState: string;
}) {
  const { data } = useMe();
  const { data: keys } = useSSHLoginKeys();
  const enabled = data?.prefs.ssh_enabled ?? false;
  const hasKeys = (keys?.length ?? 0) > 0;
  const running = machineState === 'running';

  return (
    <section className="network-policy-panel">
      <h3>SSH</h3>
      {!enabled ? (
        <p className="muted">
          SSH is off for your account. Turn on <strong>Allow SSH into my machines</strong> under
          Settings → SSH access to connect from a local terminal.
        </p>
      ) : (
        <>
          <p className="muted">
            Connect from a local terminal with the <code>proteos</code> CLI. This machine has no
            inbound port — the connection is tunnelled through ProteOS.
          </p>
          <CopyBlock text={`proteos ssh ${machineId}`} />
          {!hasKeys && (
            <p className="ssh-notice">
              You have no SSH keys registered yet, so logging in will be refused. Add one under
              Settings → SSH access.
            </p>
          )}
          {!running && (
            <p className="muted">Start the machine before connecting — SSH needs it running.</p>
          )}
        </>
      )}
    </section>
  );
}

// CopyBlock renders a command with a copy button — these are meant to be taken,
// not read.
function CopyBlock({ text }: { text: string }) {
  const [copied, setCopied] = useState(false);
  const onCopy = () => {
    void navigator.clipboard?.writeText(text);
    setCopied(true);
    setTimeout(() => setCopied(false), 1500);
  };
  return (
    <div className="ssh-copy-block">
      <pre className="ssh-command">{text}</pre>
      <button type="button" className="btn-ghost ssh-copy-btn" onClick={onCopy}>
        {copied ? 'Copied' : 'Copy'}
      </button>
    </div>
  );
}
