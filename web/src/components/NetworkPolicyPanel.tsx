import { useEffect, useState } from 'react';
import { ApiError } from '../api/client';
import type { NetworkPolicyMode } from '../api/client';
import { useNetworkPolicy, useSetNetworkPolicy } from '../api/hooks';

const MODE_LABELS: Record<NetworkPolicyMode, string> = {
  allow_all: 'Allow all',
  deny_all: 'Deny all',
  allow_domains: 'Allow domains',
  deny_domains: 'Deny domains',
};
const MODES = Object.keys(MODE_LABELS) as NetworkPolicyMode[];

// NetworkPolicyPanel (TAV-116) configures a machine's network access: one of
// four modes, with a domain allow/deny list editor for the two domain-list
// modes. Changes apply the next time the machine (re)boots, not immediately to
// an already-running one (see the control-plane handler).
export function NetworkPolicyPanel({ machineId }: { machineId: string }) {
  const { data: policy, isLoading } = useNetworkPolicy(machineId);
  const setPolicy = useSetNetworkPolicy(machineId);

  const [mode, setMode] = useState<NetworkPolicyMode>('allow_all');
  const [domains, setDomains] = useState<string[]>([]);
  const [domainInput, setDomainInput] = useState('');
  const [dirty, setDirty] = useState(false);

  // Seed local edit state from the server whenever the loaded policy changes
  // (first load, or a save's refetch) — but not on every render, so in-progress
  // edits aren't clobbered by an unrelated background refetch.
  useEffect(() => {
    if (!policy) return;
    setMode(policy.mode);
    setDomains(policy.domains);
    setDirty(false);
  }, [policy]);

  const showDomains = mode === 'allow_domains' || mode === 'deny_domains';

  const addDomain = () => {
    const d = domainInput.trim();
    if (!d || domains.includes(d)) return;
    setDomains([...domains, d]);
    setDomainInput('');
    setDirty(true);
  };

  const removeDomain = (d: string) => {
    setDomains(domains.filter((x) => x !== d));
    setDirty(true);
  };

  const onSave = () => {
    setPolicy.mutate({ mode, domains }, { onSuccess: () => setDirty(false) });
  };

  const saveError =
    setPolicy.error instanceof ApiError && setPolicy.error.code === 'invalid_network_policy'
      ? 'Check the domain list — one of the entries looks malformed (plain hostnames only, e.g. github.com).'
      : 'Could not save the network policy.';

  return (
    <section className="network-policy-panel">
      <h3>Network policy</h3>
      <p className="muted">
        Controls this machine&apos;s network access. Changes apply the next time it starts.
      </p>
      {isLoading ? (
        <p className="muted">Loading…</p>
      ) : (
        <>
          <label className="provider-field">
            <span className="provider-field-label">Mode</span>
            <select
              className="provider-key-input"
              value={mode}
              onChange={(e) => {
                setMode(e.target.value as NetworkPolicyMode);
                setDirty(true);
              }}
            >
              {MODES.map((m) => (
                <option key={m} value={m}>
                  {MODE_LABELS[m]}
                </option>
              ))}
            </select>
          </label>

          {showDomains && (
            <div className="network-policy-domains">
              <div className="network-policy-domain-add">
                <input
                  className="provider-key-input"
                  placeholder="example.com"
                  value={domainInput}
                  onChange={(e) => setDomainInput(e.target.value)}
                  onKeyDown={(e) => {
                    if (e.key === 'Enter') {
                      e.preventDefault();
                      addDomain();
                    }
                  }}
                />
                <button
                  type="button"
                  className="btn"
                  onClick={addDomain}
                  disabled={!domainInput.trim()}
                >
                  Add
                </button>
              </div>
              {domains.length === 0 ? (
                <p className="muted">No domains yet.</p>
              ) : (
                <ul className="network-policy-domain-list">
                  {domains.map((d) => (
                    <li key={d} className="network-policy-domain-row">
                      <code className="mono">{d}</code>
                      <button type="button" className="btn-ghost" onClick={() => removeDomain(d)}>
                        Remove
                      </button>
                    </li>
                  ))}
                </ul>
              )}
            </div>
          )}

          <div className="provider-row-actions">
            <button
              className="btn-primary"
              type="button"
              onClick={onSave}
              disabled={!dirty || setPolicy.isPending}
            >
              {setPolicy.isPending ? 'Saving…' : 'Save'}
            </button>
          </div>
          {setPolicy.isError && <span className="error-inline">{saveError}</span>}
        </>
      )}
    </section>
  );
}
