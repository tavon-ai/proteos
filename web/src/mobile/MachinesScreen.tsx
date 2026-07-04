import { useState } from 'react';
import type { MachineState, MachineSummary, MachineTemplate } from '../api/client';
import { useMachineMutations, useTemplates } from '../api/hooks';
import { CloseIcon, PlayIcon, StopIcon } from './icons';

// Transitional states render an amber dot + spinner instead of a control.
const transitional: MachineState[] = [
  'requested',
  'provisioning',
  'starting',
  'stopping',
  'hibernating',
];

function isTransitional(state: MachineState): boolean {
  return transitional.includes(state);
}

// gib renders a MiB value as a compact GiB string (2048 → "2 GiB").
function gib(mib: number): string {
  const g = mib / 1024;
  return `${Number.isInteger(g) ? g : g.toFixed(1)} GiB`;
}

function stateLabel(state: MachineState): string {
  switch (state) {
    case 'running':
      return 'Running';
    case 'stopped':
      return 'Stopped';
    case 'error':
      return 'Error';
    case 'stopping':
      return 'Stopping…';
    case 'hibernating':
      return 'Hibernating…';
    default: // requested / provisioning / starting
      return 'Starting…';
  }
}

// MachinesScreen is the glance-and-toggle surface: live machine cards with one
// start/stop control each, plus the full-screen create sheet.
export function MachinesScreen({ machines }: { machines: MachineSummary[] }) {
  const { start, stop } = useMachineMutations();
  const [creating, setCreating] = useState(false);

  return (
    <div className="m-screen">
      <header className="m-header m-header-row">
        <h1 className="m-title">Machines</h1>
        <button type="button" className="m-new-btn" onClick={() => setCreating(true)}>
          ＋ New
        </button>
      </header>
      <div className="m-body">
        <div className="m-machine-list">
          {machines.length === 0 && (
            <p className="m-empty">No machines yet. Create one to get started.</p>
          )}
          {machines.map((m) => (
            <MachineCard
              key={m.id}
              machine={m}
              onStart={() => start.mutate(m.id)}
              onStop={() => stop.mutate(m.id)}
            />
          ))}
        </div>
      </div>
      {creating && <CreateMachineSheet onClose={() => setCreating(false)} />}
    </div>
  );
}

function MachineCard({
  machine,
  onStart,
  onStop,
}: {
  machine: MachineSummary;
  onStart: () => void;
  onStop: () => void;
}) {
  const busy = isTransitional(machine.state);
  const spec = machine.resource_spec;
  const disk = machine.disk_mib ?? spec.disk_mib;
  // A transitional card shows only the state ("Starting…", amber) per the
  // design; the resource specs return once the machine settles.
  const meta = busy
    ? stateLabel(machine.state)
    : [
        stateLabel(machine.state),
        `${spec.vcpus} vCPU`,
        gib(spec.mem_mib),
        ...(disk ? [gib(disk)] : []),
      ].join(' · ');

  return (
    <div className={`m-machine-card${busy ? ' is-busy' : ''}`}>
      <span className={`m-dot m-dot-${busy ? 'busy' : machine.state}`} />
      <div className="m-machine-text">
        <div className="m-machine-name">{machine.name}</div>
        <div className={`m-machine-meta${busy ? ' is-busy' : ''}`}>{meta}</div>
      </div>
      {busy ? (
        <span className="m-spinner" role="status" aria-label={stateLabel(machine.state)} />
      ) : machine.state === 'running' ? (
        <button
          type="button"
          className="m-ctl m-ctl-stop"
          aria-label="Stop machine"
          onClick={onStop}
        >
          <StopIcon size={16} />
        </button>
      ) : (
        <button
          type="button"
          className="m-ctl m-ctl-start"
          aria-label="Start machine"
          onClick={onStart}
        >
          <PlayIcon size={16} />
        </button>
      )}
    </div>
  );
}

// CreateMachineSheet is the mobile "＋ New" flow: the desktop template picker
// restyled as a full-screen sheet (name + template; resources stay at the
// template's defaults — overrides remain a desktop affordance).
function CreateMachineSheet({ onClose }: { onClose: () => void }) {
  const { data: templates = [] } = useTemplates();
  const { create } = useMachineMutations();
  const [name, setName] = useState('');
  const [templateId, setTemplateId] = useState<string | null>(null);

  const submit = (e: { preventDefault(): void }) => {
    e.preventDefault();
    create.mutate(
      { name: name.trim() || undefined, template_id: templateId ?? templates[0]?.id },
      { onSuccess: onClose },
    );
  };

  return (
    <div className="m-sheet" role="dialog" aria-modal="true" aria-label="New machine">
      <header className="m-header m-header-row">
        <h1 className="m-title">New machine</h1>
        <button type="button" className="m-icon-btn" aria-label="Close" onClick={onClose}>
          <CloseIcon size={20} />
        </button>
      </header>
      <form className="m-body m-sheet-form" onSubmit={submit}>
        <label className="m-field">
          <span className="m-field-label">Name (optional)</span>
          <input
            className="m-input"
            type="text"
            value={name}
            placeholder="auto-named"
            maxLength={64}
            onChange={(e) => setName(e.target.value)}
          />
        </label>
        <div className="m-field">
          <span className="m-field-label">Template</span>
          {templates.length === 0 ? (
            <p className="m-empty">No templates available; a default machine will be created.</p>
          ) : (
            <div role="radiogroup" aria-label="Template" className="m-template-list">
              {templates.map((t) => (
                <TemplateOption
                  key={t.id}
                  template={t}
                  selected={(templateId ?? templates[0]?.id) === t.id}
                  onSelect={() => setTemplateId(t.id)}
                />
              ))}
            </div>
          )}
        </div>
        {create.isError && (
          <div className="m-error">Could not create the machine. Please try again.</div>
        )}
        <button type="submit" className="m-primary-btn" disabled={create.isPending}>
          {create.isPending ? 'Creating…' : 'Create machine'}
        </button>
      </form>
    </div>
  );
}

function TemplateOption({
  template,
  selected,
  onSelect,
}: {
  template: MachineTemplate;
  selected: boolean;
  onSelect: () => void;
}) {
  return (
    <label className={`m-template-option${selected ? ' is-selected' : ''}`}>
      <input type="radio" name="template" checked={selected} onChange={onSelect} />
      <span className="m-machine-text">
        <span className="m-machine-name">{template.label}</span>
        {template.description && <span className="m-machine-meta">{template.description}</span>}
        <span className="m-machine-meta">
          {template.defaults.vcpus} vCPU · {gib(template.defaults.mem_mib)} ·{' '}
          {gib(template.defaults.disk_mib)}
        </span>
      </span>
    </label>
  );
}
