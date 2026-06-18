import { useEffect, useState } from 'react';
import { ApiError, type MachineState, type Me } from '../api/client';
import { useMachineMutations } from '../api/hooks';
import { useSelectedMachine } from './selectedMachine';
import { useWindowManager } from './windowManagerContext';
import { openHomeTerminal, openLogs, openProjects, openSettings } from './openers';

const TRANSITIONAL: ReadonlySet<MachineState> = new Set([
  'requested',
  'provisioning',
  'starting',
  'stopping',
  'hibernating',
]);

// Taskbar is the top bar of the desktop: the brand, the machine switcher with
// start/stop/destroy controls for the active machine, quick-launch buttons for
// the Projects/Terminal/Settings/Activity windows, a clock, and the user menu.
export function Taskbar({ me, onLogout, loggingOut }: { me: Me; onLogout: () => void; loggingOut: boolean }) {
  const wm = useWindowManager();
  const clock = useClock();

  return (
    <header className="taskbar">
      <div className="taskbar-left">
        <span className="brand">ProteOS</span>
        <MachineSwitcher />
      </div>

      <nav className="taskbar-apps">
        <button className="taskbar-app" onClick={() => openProjects(wm)}>
          Projects
        </button>
        <button className="taskbar-app" onClick={() => openHomeTerminal(wm)}>
          Terminal
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

// MachineSwitcher shows the active machine's state badge and lifecycle controls,
// plus a dropdown to switch between machines, create new ones (auto-named, up to
// the per-user cap), and rename the active one.
function MachineSwitcher() {
  const { machines, selected, selectedId, setSelectedId } = useSelectedMachine();
  const { create, start, stop, destroy, rename } = useMachineMutations();
  const [open, setOpen] = useState(false);

  const state = selected?.state;
  const busy =
    (state ? TRANSITIONAL.has(state) : false) ||
    create.isPending ||
    start.isPending ||
    stop.isPending ||
    destroy.isPending;

  const atLimit = create.error instanceof ApiError && create.error.code === 'machine_limit';

  const onCreate = () => {
    create.mutate(undefined, {
      onSuccess: (m) => {
        setSelectedId(m.id);
        setOpen(false);
      },
    });
  };

  const onRename = () => {
    if (!selected) return;
    const name = window.prompt('Rename machine', selected.name);
    if (name && name.trim()) rename.mutate({ id: selected.id, name: name.trim() });
  };

  const onDestroy = () => {
    if (!selectedId) return;
    if (window.confirm('Destroy this machine? Its persistent disk is wiped and cannot be recovered.')) {
      destroy.mutate(selectedId);
    }
  };

  return (
    <div className="machine-switcher">
      <button
        className="machine-switcher-toggle"
        onClick={() => setOpen((v) => !v)}
        aria-haspopup="listbox"
        aria-expanded={open}
      >
        {selected ? (
          <>
            <span className={`badge badge-${selected.state}`}>{selected.state}</span>
            <span className="machine-switcher-name">{selected.name}</span>
          </>
        ) : (
          <span className="badge badge-stopped">no machine</span>
        )}
        <span aria-hidden>▾</span>
      </button>

      {state && TRANSITIONAL.has(state) && <span className="spinner" aria-label="working" />}

      {/* Lifecycle controls for the active machine. */}
      {(state === 'stopped' || state === 'error') && (
        <button className="btn-secondary" onClick={() => selectedId && start.mutate(selectedId)} disabled={busy}>
          Start
        </button>
      )}
      {state === 'running' && (
        <button className="btn-secondary" onClick={() => selectedId && stop.mutate(selectedId)} disabled={busy}>
          Stop
        </button>
      )}
      {selected && (
        <button className="btn-danger" onClick={onDestroy} disabled={busy}>
          {destroy.isPending ? 'Destroying…' : 'Destroy'}
        </button>
      )}

      {open && (
        <>
          <div className="machine-menu-backdrop" onClick={() => setOpen(false)} />
          <div className="machine-menu" role="listbox">
            {machines.length === 0 && <div className="machine-menu-empty">No machines yet</div>}
            {machines.map((m) => (
              <button
                key={m.id}
                className={`machine-menu-item${m.id === selectedId ? ' is-selected' : ''}`}
                role="option"
                aria-selected={m.id === selectedId}
                onClick={() => {
                  setSelectedId(m.id);
                  setOpen(false);
                }}
              >
                <span className={`badge badge-${m.state}`}>{m.state}</span>
                <span className="machine-menu-name">{m.name}</span>
              </button>
            ))}
            <div className="machine-menu-sep" />
            {selected && (
              <button className="machine-menu-action" onClick={onRename}>
                Rename “{selected.name}”
              </button>
            )}
            <button className="machine-menu-action" onClick={onCreate} disabled={create.isPending}>
              {create.isPending ? 'Creating…' : '+ New machine'}
            </button>
            {atLimit && <div className="machine-menu-error">Machine limit reached</div>}
          </div>
        </>
      )}
    </div>
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
