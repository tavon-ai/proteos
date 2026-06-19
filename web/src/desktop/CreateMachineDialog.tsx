import { useEffect, useMemo, useState } from 'react';
import { ApiError, type MachineSummary, type MachineTemplate } from '../api/client';
import { useMachineMutations } from '../api/hooks';
import { Modal } from './Modal';

// gib renders a MiB value as a compact GiB hint (e.g. 2048 → "2 GiB").
function gib(mib: number): string {
  const g = mib / 1024;
  return `${Number.isInteger(g) ? g : g.toFixed(1)} GiB`;
}

// CreateMachineDialog is the new-machine flow: pick a template, optionally name
// it, and (under Advanced) override the template's default resources within the
// caps the catalog reports. Submits POST /api/machines and reports machine_limit
// / unknown_template / invalid_resources inline.
export function CreateMachineDialog({
  templates,
  onClose,
  onCreated,
}: {
  templates: MachineTemplate[];
  onClose: () => void;
  onCreated: (m: MachineSummary) => void;
}) {
  const { create } = useMachineMutations();
  const [templateId, setTemplateId] = useState(templates[0]?.id ?? '');
  const [name, setName] = useState('');
  const [advanced, setAdvanced] = useState(false);

  const selected = useMemo(
    () => templates.find((t) => t.id === templateId) ?? templates[0],
    [templates, templateId],
  );

  const [vcpus, setVcpus] = useState(selected?.defaults.vcpus ?? 2);
  const [memMib, setMemMib] = useState(selected?.defaults.mem_mib ?? 2048);
  const [diskMib, setDiskMib] = useState(selected?.defaults.disk_mib ?? 10240);

  // Picking a template resets the resource fields to its defaults — the template
  // is the starting point, overrides layer on top.
  useEffect(() => {
    if (!selected) return;
    setVcpus(selected.defaults.vcpus);
    setMemMib(selected.defaults.mem_mib);
    setDiskMib(selected.defaults.disk_mib);
  }, [selected]);

  const limits = selected?.limits;

  const submit = (e: React.FormEvent) => {
    e.preventDefault();
    create.mutate(
      {
        name: name.trim() || undefined,
        template_id: templateId || undefined,
        vcpus,
        mem_mib: memMib,
        disk_mib: diskMib,
      },
      { onSuccess: onCreated },
    );
  };

  const err = create.error;
  let errMsg: string | null = null;
  if (err instanceof ApiError) {
    if (err.code === 'machine_limit') errMsg = 'You have reached your machine limit.';
    else if (err.code === 'unknown_template') errMsg = 'That template is no longer available.';
    else if (err.code === 'invalid_resources')
      errMsg = err.detail ?? 'Requested resources are out of range.';
    else errMsg = 'Could not create the machine. Please try again.';
  } else if (err) {
    errMsg = 'Could not create the machine. Please try again.';
  }

  return (
    <Modal title="New machine" onClose={onClose}>
      <form className="create-machine" onSubmit={submit}>
        <label className="field">
          <span className="field-label">Name (optional)</span>
          <input
            className="field-input"
            type="text"
            value={name}
            placeholder="auto-named"
            onChange={(e) => setName(e.target.value)}
            maxLength={64}
          />
        </label>

        <div className="field">
          <span className="field-label">Template</span>
          {templates.length === 0 ? (
            <p className="field-hint">No templates available; a default machine will be created.</p>
          ) : (
            <div className="template-list" role="radiogroup" aria-label="Template">
              {templates.map((t) => (
                <label
                  key={t.id}
                  className={`template-option${t.id === templateId ? ' is-selected' : ''}`}
                >
                  <input
                    type="radio"
                    name="template"
                    value={t.id}
                    checked={t.id === templateId}
                    onChange={() => setTemplateId(t.id)}
                  />
                  <span className="template-option-body">
                    <span className="template-option-label">{t.label}</span>
                    {t.description && <span className="template-option-desc">{t.description}</span>}
                    <span className="template-option-specs">
                      {t.defaults.vcpus} vCPU · {gib(t.defaults.mem_mib)} RAM ·{' '}
                      {gib(t.defaults.disk_mib)} disk
                    </span>
                  </span>
                </label>
              ))}
            </div>
          )}
        </div>

        {selected && (
          <div className="field">
            <button
              type="button"
              className="advanced-toggle"
              aria-expanded={advanced}
              onClick={() => setAdvanced((v) => !v)}
            >
              {advanced ? '▾' : '▸'} Advanced — resources
            </button>
            {advanced && limits && (
              <div className="resource-grid">
                <ResourceInput
                  label="vCPUs"
                  value={vcpus}
                  min={limits.vcpus.min}
                  max={limits.vcpus.max}
                  step={1}
                  onChange={setVcpus}
                />
                <ResourceInput
                  label="Memory (MiB)"
                  hint={gib(memMib)}
                  value={memMib}
                  min={limits.mem_mib.min}
                  max={limits.mem_mib.max}
                  step={512}
                  onChange={setMemMib}
                />
                <ResourceInput
                  label="Disk (MiB)"
                  hint={gib(diskMib)}
                  value={diskMib}
                  min={limits.disk_mib.min}
                  max={limits.disk_mib.max}
                  step={1024}
                  onChange={setDiskMib}
                />
                <p className="field-hint resource-note">
                  Disk size is fixed for the machine&rsquo;s life and cannot be changed later.
                </p>
              </div>
            )}
          </div>
        )}

        {errMsg && <div className="form-error">{errMsg}</div>}

        <div className="modal-actions">
          <button type="button" className="btn-ghost" onClick={onClose} disabled={create.isPending}>
            Cancel
          </button>
          <button type="submit" className="btn-primary" disabled={create.isPending}>
            {create.isPending ? 'Creating…' : 'Create machine'}
          </button>
        </div>
      </form>
    </Modal>
  );
}

// ResourceInput is a labelled, bounded number input. The browser enforces
// min/max/step on the spinner; the backend re-validates authoritatively.
function ResourceInput({
  label,
  hint,
  value,
  min,
  max,
  step,
  onChange,
}: {
  label: string;
  hint?: string;
  value: number;
  min: number;
  max: number;
  step: number;
  onChange: (v: number) => void;
}) {
  return (
    <label className="field">
      <span className="field-label">
        {label}
        {hint && <span className="field-hint-inline"> ≈ {hint}</span>}
      </span>
      <input
        className="field-input"
        type="number"
        value={value}
        min={min}
        max={max}
        step={step}
        onChange={(e) => onChange(Number(e.target.value))}
      />
      <span className="field-hint">
        {min}–{max}
      </span>
    </label>
  );
}
