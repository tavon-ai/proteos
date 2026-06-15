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
  // One value per declared secret field, keyed by field name. Rendered entirely
  // from provider.secret_fields — no per-provider code (Phase 6 decision #5).
  const [values, setValues] = useState<Record<string, string>>({});

  const setField = (name: string, v: string) =>
    setValues((prev) => ({ ...prev, [name]: v }));

  const reset = () => {
    setValues({});
    setEditing(false);
  };

  // Every declared field must be non-empty before we submit.
  const complete = provider.secret_fields.every((f) => (values[f.name] ?? "").trim() !== "");

  const onSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    if (!complete) return;
    const fields: Record<string, string> = {};
    for (const f of provider.secret_fields) fields[f.name] = values[f.name].trim();
    setKey.mutate({ key: provider.key, fields }, { onSuccess: reset });
  };

  const onDelete = () => {
    if (!window.confirm(`Remove your ${provider.display_name} key?`)) return;
    deleteKey.mutate(provider.key);
  };

  const verb = provider.key_set ? "Replace" : "Set";

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
          {provider.secret_fields.map((field, i) => (
            <label key={field.name} className="provider-field">
              <span className="provider-field-label">{field.label}</span>
              <input
                type="password"
                className="provider-key-input"
                placeholder={field.label}
                value={values[field.name] ?? ""}
                autoFocus={i === 0}
                autoComplete="off"
                spellCheck={false}
                onChange={(e) => setField(field.name, e.target.value)}
              />
            </label>
          ))}
          <div className="provider-row-actions">
            <button className="btn" type="submit" disabled={!complete || setKey.isPending}>
              {setKey.isPending ? "Saving…" : "Save"}
            </button>
            <button className="btn-ghost" type="button" onClick={reset}>
              Cancel
            </button>
          </div>
          {setKey.isError && <span className="error-inline">Could not save key.</span>}
        </form>
      ) : (
        <div className="provider-row-actions">
          <button className="btn" onClick={() => setEditing(true)} disabled={!provider.enabled}>
            {verb} key
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
