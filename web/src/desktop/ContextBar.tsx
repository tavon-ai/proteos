import { useEffect, useRef, useState } from 'react';
import { type MachineState } from '../api/client';
import { useMachineMutations, useProjects, useTemplates } from '../api/hooks';
import { useSelectedMachine } from './selectedMachineStore';
import { useWindowManager } from './windowManagerContext';
import { focusedWindow } from './windowState';
import { useClickOutside } from './useClickOutside';
import { CreateMachineDialog } from './CreateMachineDialog';
import { DestroyAllDialog } from './DestroyAllDialog';
import { MachineDetails } from './MachineDetails';

const TRANSITIONAL: ReadonlySet<MachineState> = new Set([
  'requested',
  'provisioning',
  'starting',
  'stopping',
  'hibernating',
]);

// pillState groups machine states into the pill's four visual treatments.
function pillState(state: MachineState | undefined): 'running' | 'stopped' | 'error' | 'busy' {
  if (!state) return 'stopped';
  if (TRANSITIONAL.has(state)) return 'busy';
  if (state === 'running' || state === 'error') return state;
  return 'stopped';
}

// ContextBar is the slim top bar of the desktop: the machine pill (the one
// machine control — switching, lifecycle, and details all live in its menu, so
// Stop/Destroy are no longer one mis-click away in the chrome), the repo/branch
// breadcrumb of the focused project window, the ⌘K search button, and a clock.
export function ContextBar({ onOpenPalette }: { onOpenPalette: () => void }) {
  const clock = useClock();

  return (
    <header className="context-bar">
      <MachinePill />
      <Breadcrumb />
      <button className="search-btn" onClick={onOpenPalette}>
        <svg width="14" height="14" viewBox="0 0 24 24" fill="none" aria-hidden>
          <circle cx="11" cy="11" r="7" stroke="currentColor" strokeWidth="1.8" />
          <path d="M20 20l-3.5-3.5" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" />
        </svg>
        <span className="search-btn-label">Search…</span>
        <kbd className="search-btn-kbd">⌘K</kbd>
      </button>
      <span className="clock">{clock}</span>
    </header>
  );
}

// MachinePill shows the active machine's name behind a state-colored pill; its
// menu switches machines and carries Details/Rename/New plus the lifecycle
// actions (Start/Stop and the confirm-guarded Destroy).
function MachinePill() {
  const { machines, selected, selectedId, setSelectedId } = useSelectedMachine();
  const { start, stop, destroy, rename } = useMachineMutations();
  const { data: templates = [] } = useTemplates();
  const [open, setOpen] = useState(false);
  const [modal, setModal] = useState<'none' | 'create' | 'details' | 'destroy-all'>('none');
  const wrapRef = useRef<HTMLDivElement | null>(null);
  useClickOutside(open, [wrapRef], () => setOpen(false));

  const state = selected?.state;
  const busy =
    (state ? TRANSITIONAL.has(state) : false) ||
    start.isPending ||
    stop.isPending ||
    destroy.isPending;

  const onRename = () => {
    if (!selected) return;
    const name = window.prompt('Rename machine', selected.name);
    if (name && name.trim()) rename.mutate({ id: selected.id, name: name.trim() });
  };

  const onDestroy = () => {
    if (!selectedId) return;
    if (
      window.confirm('Destroy this machine? Its persistent disk is wiped and cannot be recovered.')
    ) {
      destroy.mutate(selectedId);
    }
    setOpen(false);
  };

  return (
    <div className="machine-pill-wrap" ref={wrapRef}>
      <button
        className={`machine-pill machine-pill-${pillState(state)}`}
        onClick={() => setOpen((v) => !v)}
        aria-haspopup="menu"
        aria-expanded={open}
        title={selected ? `${selected.name} — ${selected.state}` : 'No machine'}
      >
        <span className="machine-pill-dot" aria-hidden />
        <span className="machine-pill-name">{selected ? selected.name : 'no machine'}</span>
        {state && TRANSITIONAL.has(state) && <span className="spinner" aria-label="working" />}
        <svg width="12" height="12" viewBox="0 0 24 24" fill="none" aria-hidden>
          <path
            d="M6 9l6 6 6-6"
            stroke="#8b949e"
            strokeWidth="2"
            strokeLinecap="round"
            strokeLinejoin="round"
          />
        </svg>
      </button>

      {open && (
        <div className="machine-menu" role="menu">
          {machines.length === 0 && <div className="machine-menu-empty">No machines yet</div>}
          {machines.map((m) => (
            <button
              key={m.id}
              className={`machine-menu-item${m.id === selectedId ? ' is-selected' : ''}`}
              role="menuitemradio"
              aria-checked={m.id === selectedId}
              onClick={() => {
                setSelectedId(m.id);
                setOpen(false);
              }}
            >
              <span className={`badge badge-${m.state}`}>{m.state}</span>
              <span className="machine-menu-name-col">
                <span className="machine-menu-name">{m.name}</span>
                {m.state === 'error' && m.last_error && (
                  <span className="machine-menu-error-reason" title={m.last_error}>
                    {m.last_error}
                  </span>
                )}
              </span>
            </button>
          ))}
          <div className="machine-menu-sep" />
          {selected && (
            <button
              className="machine-menu-action"
              onClick={() => {
                setModal('details');
                setOpen(false);
              }}
            >
              Details
            </button>
          )}
          {selected && (
            <button className="machine-menu-action" onClick={onRename}>
              Rename “{selected.name}”
            </button>
          )}
          <button
            className="machine-menu-action"
            onClick={() => {
              setModal('create');
              setOpen(false);
            }}
          >
            + New machine
          </button>
          {machines.length > 0 && (
            <button
              className="machine-menu-action machine-menu-danger"
              onClick={() => {
                setModal('destroy-all');
                setOpen(false);
              }}
            >
              Destroy all machines ({machines.length})…
            </button>
          )}
          {selected && (
            <>
              <div className="machine-menu-sep" />
              {(state === 'stopped' || state === 'error') && (
                <button
                  className="machine-menu-action"
                  onClick={() => {
                    if (selectedId) start.mutate(selectedId);
                    setOpen(false);
                  }}
                  disabled={busy}
                >
                  Start
                </button>
              )}
              {state === 'running' && (
                <button
                  className="machine-menu-action"
                  onClick={() => {
                    if (selectedId) stop.mutate(selectedId);
                    setOpen(false);
                  }}
                  disabled={busy}
                >
                  Stop
                </button>
              )}
              <button
                className="machine-menu-action machine-menu-danger"
                onClick={onDestroy}
                disabled={busy}
              >
                {destroy.isPending ? 'Destroying…' : 'Destroy…'}
              </button>
            </>
          )}
        </div>
      )}

      {modal === 'create' && (
        <CreateMachineDialog
          templates={templates}
          onClose={() => setModal('none')}
          onCreated={(m) => {
            setSelectedId(m.id);
            setModal('none');
          }}
        />
      )}
      {modal === 'details' && selected && (
        <MachineDetails machine={selected} templates={templates} onClose={() => setModal('none')} />
      )}
      {modal === 'destroy-all' && (
        <DestroyAllDialog machines={machines} onClose={() => setModal('none')} />
      )}
    </div>
  );
}

// Breadcrumb reflects the focused window's project: repo name, then the branch
// as a chip. Hidden when the focused window is not project-scoped (or belongs
// to no project we know).
function Breadcrumb() {
  const wm = useWindowManager();
  const { selected, selectedId } = useSelectedMachine();
  const running = selected?.state === 'running';
  const { data } = useProjects(selectedId, running);

  const projectId = focusedWindow(wm.windows, selectedId)?.projectId;
  const project = projectId ? data?.projects.find((p) => p.path === projectId) : undefined;
  if (!project) return null;

  return (
    <div className="breadcrumb">
      <span className="breadcrumb-repo">{project.name}</span>
      {project.branch && (
        <>
          <span className="breadcrumb-sep" aria-hidden>
            /
          </span>
          <span className="breadcrumb-branch">{project.branch}</span>
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
