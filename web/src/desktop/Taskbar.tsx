import { useEffect, useState } from 'react';
import type { MachineState, MachineSummary, Me } from '../api/client';
import { useMachineMutations } from '../api/hooks';
import { useWindowManager } from './windowManagerContext';
import { openLogs, openProjects, openSettings } from './openers';

const TRANSITIONAL: ReadonlySet<MachineState> = new Set([
  'requested',
  'provisioning',
  'starting',
  'stopping',
  'hibernating',
]);

// Taskbar is the top bar of the desktop: the brand, the machine state badge with
// start/stop controls, quick-launch buttons for the Projects/Settings/Activity
// windows, a clock, and the user menu (decision #1/#7). Machine lifecycle and the
// event stream become first-class desktop chrome here rather than a card.
export function Taskbar({
  machine,
  me,
  onLogout,
  loggingOut,
}: {
  machine: MachineSummary | null;
  me: Me;
  onLogout: () => void;
  loggingOut: boolean;
}) {
  const wm = useWindowManager();
  const { create, start, stop } = useMachineMutations();
  const clock = useClock();

  const state = machine?.state;
  const busy =
    (state ? TRANSITIONAL.has(state) : false) ||
    create.isPending ||
    start.isPending ||
    stop.isPending;

  return (
    <header className="taskbar">
      <div className="taskbar-left">
        <span className="brand">ProteOS</span>
        {machine ? (
          <span className={`badge badge-${machine.state}`}>{machine.state}</span>
        ) : (
          <span className="badge badge-stopped">no machine</span>
        )}
        {state && TRANSITIONAL.has(state) && <span className="spinner" aria-label="working" />}
        {!machine && (
          <button className="btn-secondary" onClick={() => create.mutate()} disabled={busy}>
            {create.isPending ? 'Creating…' : 'Create machine'}
          </button>
        )}
        {(state === 'stopped' || state === 'error') && (
          <button className="btn-secondary" onClick={() => start.mutate()} disabled={busy}>
            Start
          </button>
        )}
        {state === 'running' && (
          <button className="btn-secondary" onClick={() => stop.mutate()} disabled={busy}>
            Stop
          </button>
        )}
      </div>

      <nav className="taskbar-apps">
        <button className="taskbar-app" onClick={() => openProjects(wm)}>
          Projects
        </button>
        <button className="taskbar-app" onClick={() => openSettings(wm)}>
          Settings
        </button>
        <button className="taskbar-app" onClick={() => openLogs(wm)}>
          Activity
        </button>
      </nav>

      <div className="taskbar-right">
        <span className="clock">{clock}</span>
        {me.user.avatar_url && (
          <img className="avatar" src={me.user.avatar_url} alt="" width={24} height={24} />
        )}
        <span className="user-login">{me.user.login}</span>
        <button className="btn-ghost" onClick={onLogout} disabled={loggingOut}>
          {loggingOut ? 'Signing out…' : 'Sign out'}
        </button>
      </div>
    </header>
  );
}

function useClock(): string {
  const [now, setNow] = useState(() => new Date());
  useEffect(() => {
    const t = setInterval(() => setNow(new Date()), 30_000);
    return () => clearInterval(t);
  }, []);
  return now.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
}
