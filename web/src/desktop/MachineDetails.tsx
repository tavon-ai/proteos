import type { MachineSummary, MachineTemplate } from '../api/client';
import { ExposeAppPanel } from '../components/ExposeAppPanel';
import { NetworkPolicyPanel } from '../components/NetworkPolicyPanel';
import { Modal } from './Modal';

function gib(mib: number | undefined): string {
  if (mib === undefined) return '—';
  const g = mib / 1024;
  return `${Number.isInteger(g) ? g : g.toFixed(1)} GiB`;
}

function formatDate(iso: string): string {
  if (!iso) return '—';
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleString();
}

// MachineDetails is a read-only summary of one machine: which template it came
// from, its pinned resources and image, and current runtime facts. Everything
// here is fixed at create time (decision #5) except the live state/IP.
export function MachineDetails({
  machine,
  templates,
  onClose,
}: {
  machine: MachineSummary;
  templates: MachineTemplate[];
  onClose: () => void;
}) {
  const tmpl = templates.find((t) => t.id === machine.template_id);
  const templateLabel = tmpl?.label ?? machine.template_id ?? 'Legacy (pre-templates)';
  const spec = machine.resource_spec;

  const rows: [string, React.ReactNode][] = [
    ['Name', machine.name],
    ['State', <span className={`badge badge-${machine.state}`}>{machine.state}</span>],
    ['Template', templateLabel],
    ['vCPUs', spec.vcpus],
    ['Memory', `${spec.mem_mib} MiB (${gib(spec.mem_mib)})`],
    [
      'Disk',
      `${machine.disk_mib ?? spec.disk_mib ?? '—'} MiB (${gib(machine.disk_mib ?? spec.disk_mib)})`,
    ],
    ['RootFS image', <code className="mono">{machine.rootfs_ref}</code>],
    ['Guest IP', machine.guest_ip ?? '—'],
    ['Boot', machine.boot ?? '—'],
    ['Created', formatDate(machine.created_at)],
    ['Last active', machine.last_active_at ? formatDate(machine.last_active_at) : '—'],
  ];

  return (
    <Modal title={`Machine details — ${machine.name}`} onClose={onClose}>
      <dl className="details-grid">
        {rows.map(([label, value]) => (
          <div className="details-row" key={label}>
            <dt className="details-key">{label}</dt>
            <dd className="details-val">{value}</dd>
          </div>
        ))}
      </dl>
      {machine.last_error && <div className="form-error">{machine.last_error}</div>}

      <ExposeAppPanel machineId={machine.id} machineState={machine.state} />

      <NetworkPolicyPanel machineId={machine.id} />

      <div className="modal-actions">
        <button type="button" className="btn-primary" onClick={onClose}>
          Close
        </button>
      </div>
    </Modal>
  );
}
