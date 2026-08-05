import { useCallback, useEffect, useMemo, useState } from 'react';
import {
  ApiError,
  api,
  type AdminMachine,
  type AdminOverview,
  type MachineState,
} from '../api/client';
import { formatAge } from './formatAge';

// AdminWindow is the fleet console: how many machines exist, how many are in
// use, and who owns them. It is READ-ONLY by design — this is the one screen
// that shows a user machines that are not theirs, and giving it no actions
// keeps a mistakenly-granted proteos.admin role to "saw a list".
//
// It renders only for holders of that role (the rail hides the entry point),
// but the gate that matters is the server's: /api/admin/overview answers 403 to
// everyone else regardless of what the client believes.

// STATE_ORDER fixes the order the state chips appear in, so the row does not
// reshuffle between polls as the histogram's key order changes. Running leads
// because it is the number an admin is actually looking for.
const STATE_ORDER: MachineState[] = [
  'running',
  'starting',
  'stopping',
  'provisioning',
  'requested',
  'hibernating',
  'stopped',
  'error',
];

// REFRESH_MS re-reads the console while it is open. The fleet changes when
// someone creates or stops a machine, and an admin staring at a stale count is
// the failure this exists to avoid. Slow enough to be invisible on the server:
// the payload is three cheap aggregate/indexed reads.
const REFRESH_MS = 15_000;

export function AdminWindow() {
  const [data, setData] = useState<AdminOverview | null>(null);
  const [error, setError] = useState<string | null>(null);
  // Distinguishes the first paint from a background refresh: only the former
  // should replace the table with a loading line.
  const [loaded, setLoaded] = useState(false);

  const load = useCallback(async () => {
    try {
      const next = await api.adminOverview();
      setData(next);
      setError(null);
    } catch (err) {
      // A 403 here means the role was revoked while the window was open. Say so
      // plainly rather than showing an empty fleet, which would read as "no
      // machines exist" — the opposite of the truth.
      if (err instanceof ApiError) {
        setError(
          err.status === 403
            ? 'Your admin access has been revoked.'
            : `Could not load the fleet (${err.code}).`,
        );
      } else {
        setError('Could not reach the control plane.');
      }
    } finally {
      setLoaded(true);
    }
  }, []);

  useEffect(() => {
    void load();
    const t = setInterval(() => void load(), REFRESH_MS);
    return () => clearInterval(t);
  }, [load]);

  if (!loaded) {
    return (
      <div className="admin-window">
        <p className="admin-empty">Loading fleet…</p>
      </div>
    );
  }
  if (error && !data) {
    return (
      <div className="admin-window">
        <p className="admin-error" role="alert">
          {error}
        </p>
      </div>
    );
  }
  if (!data) return null;

  return (
    <div className="admin-window">
      {/* A refresh that fails while earlier data is on screen: keep the data,
          but never let it pass for current. */}
      {error && (
        <p className="admin-error" role="alert">
          {error}
        </p>
      )}

      <div className="admin-stats">
        <Stat label="Machines" value={data.totals.machines} hint="created, all states" />
        <Stat label="In use" value={data.totals.running} hint="running now" accent />
        <Stat
          label="Owners"
          value={data.totals.users_with_machines}
          hint={`of ${data.totals.users} users`}
        />
      </div>

      <div className="admin-chips">
        {STATE_ORDER.filter((s) => (data.totals.by_state[s] ?? 0) > 0).map((s) => (
          <span key={s} className={`admin-chip admin-chip-${s}`}>
            <span className="admin-chip-dot" aria-hidden />
            {s} <strong>{data.totals.by_state[s]}</strong>
          </span>
        ))}
      </div>

      <MachineTable machines={data.machines} />

      {data.truncated && (
        <p className="admin-note">
          Showing the {data.machines.length} newest machines. The totals above cover the whole
          fleet.
        </p>
      )}
    </div>
  );
}

function Stat({
  label,
  value,
  hint,
  accent,
}: {
  label: string;
  value: number;
  hint: string;
  accent?: boolean;
}) {
  return (
    <div className={accent ? 'admin-stat admin-stat-accent' : 'admin-stat'}>
      <span className="admin-stat-value">{value}</span>
      <span className="admin-stat-label">{label}</span>
      <span className="admin-stat-hint">{hint}</span>
    </div>
  );
}

function MachineTable({ machines }: { machines: AdminMachine[] }) {
  // Group by owner so the table answers "who owns them" by shape, not by the
  // reader scanning a flat list for repeated names. Owners are ordered by
  // machine count (the heaviest users first), which is what an admin looking at
  // capacity wants at the top.
  const groups = useMemo(() => {
    const byOwner = new Map<string, { owner: AdminMachine['owner']; machines: AdminMachine[] }>();
    for (const m of machines) {
      const g = byOwner.get(m.owner.id) ?? { owner: m.owner, machines: [] };
      g.machines.push(m);
      byOwner.set(m.owner.id, g);
    }
    return [...byOwner.values()].sort(
      (a, b) => b.machines.length - a.machines.length || a.owner.login.localeCompare(b.owner.login),
    );
  }, [machines]);

  if (machines.length === 0) {
    return <p className="admin-empty">No machines have been created yet.</p>;
  }

  return (
    <div className="admin-table-wrap">
      <table className="admin-table">
        <thead>
          <tr>
            <th scope="col">Machine</th>
            <th scope="col">State</th>
            <th scope="col">Size</th>
            <th scope="col">Host</th>
            <th scope="col">Created</th>
            <th scope="col">Last active</th>
          </tr>
        </thead>
        {groups.map((g) => (
          <tbody key={g.owner.id}>
            <tr className="admin-owner-row">
              {/* colSpan spans the whole table: the owner is a section heading
                  for the rows beneath it, not a cell in the first column. */}
              <th colSpan={6} scope="colgroup">
                <span className="admin-owner">
                  {g.owner.avatar_url && (
                    <img src={g.owner.avatar_url} alt="" width={18} height={18} />
                  )}
                  <span className="admin-owner-login">{g.owner.login}</span>
                  <span className="admin-owner-email">{g.owner.email}</span>
                  <span className="admin-owner-count">
                    {g.machines.length} machine{g.machines.length === 1 ? '' : 's'},{' '}
                    {g.machines.filter((m) => m.state === 'running').length} running
                  </span>
                </span>
              </th>
            </tr>
            {g.machines.map((m) => (
              <tr key={m.id}>
                <td>
                  <span className="admin-machine-name">{m.name || '(unnamed)'}</span>
                  {m.last_error && (
                    <span className="admin-machine-error" title={m.last_error}>
                      {m.last_error}
                    </span>
                  )}
                </td>
                <td>
                  <span className={`admin-chip admin-chip-${m.state}`}>
                    <span className="admin-chip-dot" aria-hidden />
                    {m.state}
                  </span>
                </td>
                <td className="admin-num">
                  {m.resource_spec.vcpus} vCPU · {Math.round(m.resource_spec.mem_mib / 1024)} GiB
                </td>
                <td>{m.host ?? <span className="admin-dim">unscheduled</span>}</td>
                <td>
                  <RelativeTime iso={m.created_at} />
                </td>
                <td>
                  {m.last_active_at ? (
                    <RelativeTime iso={m.last_active_at} />
                  ) : (
                    <span className="admin-dim">never</span>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        ))}
      </table>
    </div>
  );
}

// RelativeTime shows "3d ago" with the exact timestamp on hover. An admin
// scanning for stale machines reads elapsed time far faster than absolute
// dates, but the exact value has to stay reachable for anything precise.
function RelativeTime({ iso }: { iso: string }) {
  const ms = Date.parse(iso);
  if (Number.isNaN(ms)) return <span className="admin-dim">—</span>;
  return (
    <time dateTime={iso} title={new Date(ms).toLocaleString()}>
      {formatAge(Date.now() - ms)}
    </time>
  );
}
