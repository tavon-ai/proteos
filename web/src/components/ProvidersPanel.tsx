import { useState } from "react";
import type { Provider } from "../api/client";
import { useProviderMutations, useProviders } from "../api/hooks";

// ProvidersPanel lists the AI providers and lets the user set (write-only),
// replace, or delete their API key per provider. The key is never rendered back:
// the panel only shows a "Key set" / "No key" badge derived from the server's
// key_set, matching the Phase 5 acceptance criterion (the key never appears in
// the React app after submission).
export function ProvidersPanel() {
  const { data: providers, isLoading, isError } = useProviders();

  return (
    <section className="providers-panel">
      <h2>AI providers</h2>
      <p className="muted">
        Paste an API key to enable a coding agent. Keys are stored encrypted and
        injected into your machine at runtime — they are never shown again.
      </p>

      {isLoading && <p className="muted">Loading providers…</p>}
      {isError && <p className="error-banner">Could not load providers.</p>}

      {providers && providers.length === 0 && (
        <p className="muted">No providers are registered.</p>
      )}

      <ul className="provider-list">
        {providers?.map((p) => (
          <ProviderRow key={p.key} provider={p} />
        ))}
      </ul>
    </section>
  );
}

function ProviderRow({ provider }: { provider: Provider }) {
  const { setKey, deleteKey } = useProviderMutations();
  const [editing, setEditing] = useState(false);
  const [value, setValue] = useState("");

  const onSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    const apiKey = value.trim();
    if (!apiKey) return;
    setKey.mutate(
      { key: provider.key, apiKey },
      {
        onSuccess: () => {
          setValue("");
          setEditing(false);
        },
      },
    );
  };

  const onDelete = () => {
    if (!window.confirm(`Remove your ${provider.display_name} API key?`)) return;
    deleteKey.mutate(provider.key);
  };

  return (
    <li className="provider-row">
      <div className="provider-row-head">
        <span className="provider-name">{provider.display_name}</span>
        {!provider.enabled && <span className="chip">disabled</span>}
        <span className={`badge ${provider.key_set ? "badge-running" : "badge-stopped"}`}>
          {provider.key_set ? "Key set" : "No key"}
        </span>
      </div>

      {editing ? (
        <form className="provider-key-form" onSubmit={onSubmit}>
          <input
            type="password"
            className="provider-key-input"
            placeholder={`${provider.display_name} API key`}
            value={value}
            autoFocus
            autoComplete="off"
            spellCheck={false}
            onChange={(e) => setValue(e.target.value)}
          />
          <button className="btn" type="submit" disabled={!value.trim() || setKey.isPending}>
            {setKey.isPending ? "Saving…" : "Save"}
          </button>
          <button
            className="btn-ghost"
            type="button"
            onClick={() => {
              setValue("");
              setEditing(false);
            }}
          >
            Cancel
          </button>
          {setKey.isError && <span className="error-inline">Could not save key.</span>}
        </form>
      ) : (
        <div className="provider-row-actions">
          <button
            className="btn"
            onClick={() => setEditing(true)}
            disabled={!provider.enabled}
          >
            {provider.key_set ? "Replace key" : "Set key"}
          </button>
          {provider.key_set && (
            <button className="btn-ghost" onClick={onDelete} disabled={deleteKey.isPending}>
              {deleteKey.isPending ? "Removing…" : "Remove"}
            </button>
          )}
        </div>
      )}
    </li>
  );
}
