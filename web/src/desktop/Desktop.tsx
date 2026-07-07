import { useEffect, useRef, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import type { MachineEvent, MachineState, MachineSummary, Me } from '../api/client';
import { useLogout, useMachineEvents, useMachines } from '../api/hooks';
import { Terminal } from '../components/Terminal';
import { EditorWindow } from '../windows/EditorWindow';
import { PreviewWindow } from '../windows/PreviewWindow';
import { ChangesWindow } from '../windows/ChangesWindow';
import { TasksWindow } from '../windows/TasksWindow';
import { LogsWindow } from '../windows/LogsWindow';
import { SettingsWindow } from '../windows/SettingsWindow';
import { Dock } from './Dock';
import { ProjectsLauncher } from './ProjectsLauncher';
import { SelectedMachineProvider } from './selectedMachine';
import { useSelectedMachine } from './selectedMachineStore';
import { LeftRail } from './LeftRail';
import { ContextBar } from './ContextBar';
import { CommandPalette } from './CommandPalette';
import { OpenAppDialog } from './OpenAppDialog';
import { openPreview } from './openers';
import { Window, WindowErrorBoundary } from './Window';
import { WindowManagerProvider } from './WindowManager';
import { useWindowManager } from './windowManagerContext';
import { openProjects } from './openers';
import { useLayoutLoader, useLayoutSaver } from './useLayout';
import type { WindowState } from './windowState';
import { WallpaperProvider } from './wallpaperContext';
import { useWallpaper } from './wallpaper';

// Desktop is the product shell: a project-centric, multi-window desktop. It owns
// the live machines + event + provider subscriptions once (a single EventSource
// backs the whole UI) and provides the active-machine selection to the tree.
export function Desktop({ me }: { me: Me }) {
  const navigate = useNavigate();
  const { data: machines } = useMachines(me.machines);
  const events = useMachineEvents(() => navigate('/login', { replace: true }));

  return (
    <WallpaperProvider>
      <SelectedMachineProvider machines={machines ?? []}>
        <DesktopScoped me={me} events={events} />
      </SelectedMachineProvider>
    </WallpaperProvider>
  );
}

// DesktopScoped binds the window manager + layout persistence to the active
// machine. The WindowManagerProvider's onChange debounce-saves that machine's
// layout to its SQLite (decision #6).
function DesktopScoped({ me, events }: { me: Me; events: MachineEvent[] }) {
  const { machines, selected, selectedId } = useSelectedMachine();
  const running = selected?.state === 'running';
  const saveLayout = useLayoutSaver(selectedId, running);

  return (
    // ONE window manager holds every machine's windows at once. Windows are
    // tagged with their machine and only the active machine's are shown (the rest
    // stay mounted but display:none), so terminals/agents keep their live PTYs and
    // scrollback across machine switches — switching is a show/hide, never a
    // remount.
    <WindowManagerProvider onChange={saveLayout}>
      <DesktopShell me={me} machines={machines ?? []} selectedId={selectedId} events={events} />
    </WindowManagerProvider>
  );
}

function DesktopShell({
  me,
  machines,
  selectedId,
  events,
}: {
  me: Me;
  machines: MachineSummary[];
  selectedId: string | null;
  events: MachineEvent[];
}) {
  const wm = useWindowManager();
  const navigate = useNavigate();
  const logout = useLogout();
  const { surfaceRef, viewport } = useSurfaceSize(wm);
  const { prefs: wallpaper } = useWallpaper();
  const [paletteOpen, setPaletteOpen] = useState(false);
  const [appDialog, setAppDialog] = useState(false);

  const selected = machines.find((m) => m.id === selectedId) ?? null;

  // ⌘K / Ctrl+K toggles the command palette from anywhere on the desktop.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'k') {
        e.preventDefault();
        setPaletteOpen((v) => !v);
      }
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, []);

  // Restore each machine's layout the first time it becomes the active running
  // machine (does not disturb other machines' live windows).
  useLayoutLoader(wm, selectedId, selected?.state === 'running');

  // Open a Projects launcher the FIRST time each machine becomes active, so a
  // machine's desktop is never empty. Tracked per-machine in a ref (not keyed on
  // wm, whose identity changes on every window mutation) so it does NOT re-open
  // or re-focus Projects on later opens/closes — closing it stays closed.
  const projectsOpened = useRef<Set<string>>(new Set());
  useEffect(() => {
    if (selectedId && !projectsOpened.current.has(selectedId)) {
      projectsOpened.current.add(selectedId);
      openProjects(wm, selectedId);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [selectedId]);

  const onLogout = () => {
    logout.mutate(undefined, {
      onSettled: () => navigate('/login', { replace: true }),
    });
  };

  const wallpaperStyle: React.CSSProperties = wallpaper.source
    ? {
        backgroundImage: `url(${wallpaper.source})`,
        backgroundSize: 'cover',
        backgroundPosition: 'center',
        backgroundRepeat: 'no-repeat',
      }
    : {};

  return (
    <div className="desktop" style={wallpaperStyle}>
      <LeftRail me={me} onLogout={onLogout} loggingOut={logout.isPending} />

      <div className="desktop-main">
        <ContextBar onOpenPalette={() => setPaletteOpen(true)} />

        <div className="desktop-surface" ref={surfaceRef}>
          {wm.windows.map((win) => {
            // A window is shown when it belongs to the active machine (or is a
            // global window with no machine). Hidden windows stay MOUNTED so their
            // terminals keep their live PTYs and scrollback across switches.
            const visible = !win.machineId || win.machineId === selectedId;
            const winMachine = win.machineId
              ? (machines.find((m) => m.id === win.machineId) ?? null)
              : selected;
            return (
              <Window key={win.id} win={win} viewport={viewport} hidden={!visible}>
                <WindowErrorBoundary onClose={() => wm.close(win.id)}>
                  <WindowBody
                    win={win}
                    machineState={winMachine?.state ?? 'stopped'}
                    lastError={winMachine?.last_error ?? null}
                    events={events}
                  />
                </WindowErrorBoundary>
              </Window>
            );
          })}

          {paletteOpen && (
            <CommandPalette
              onClose={() => setPaletteOpen(false)}
              onOpenApp={() => {
                setPaletteOpen(false);
                setAppDialog(true);
              }}
            />
          )}
        </div>

        <Dock />
      </div>

      {appDialog && selectedId && (
        <OpenAppDialog
          onClose={() => setAppDialog(false)}
          onOpen={(port) => {
            openPreview(wm, selectedId, port);
            setAppDialog(false);
          }}
        />
      )}
    </div>
  );
}

// WindowBody routes a window to its content component by kind. It is rendered
// inside <Window>, which mounts it once for the window's lifetime — so a live
// terminal or the editor iframe survives every minimize/maximize/focus.
function WindowBody({
  win,
  machineState,
  lastError,
  events,
}: {
  win: WindowState;
  machineState: MachineState;
  lastError: string | null;
  events: MachineEvent[];
}) {
  // Machine-scoped windows show the error reason instead of their normal content
  // so users know why the machine is unavailable without opening machine details.
  if (win.machineId && machineState === 'error') {
    return (
      <div className="editor-banner" role="alert">
        <p>Machine error{lastError ? `: ${lastError}` : '.'}</p>
        <p style={{ fontSize: '0.8rem' }}>Start the machine from the machine menu to reconnect.</p>
      </div>
    );
  }
  switch (win.kind) {
    case 'projects':
      return (
        <ProjectsLauncher
          machineId={win.machineId ?? null}
          machineState={machineState}
          events={events}
        />
      );
    case 'terminal':
      return win.machineId ? (
        <Terminal machineID={win.machineId} session={win.session} cwd={win.cwd} />
      ) : (
        <StoppedBody />
      );
    case 'agent':
      return win.machineId ? (
        <Terminal
          machineID={win.machineId}
          provider={win.provider}
          session={win.session}
          cwd={win.cwd}
        />
      ) : (
        <StoppedBody />
      );
    case 'editor':
      return (
        <EditorWindow
          machineId={win.machineId ?? null}
          machineState={machineState}
          folder={win.folder}
        />
      );
    case 'preview':
      return (
        <PreviewWindow
          machineId={win.machineId ?? null}
          machineState={machineState}
          port={win.port}
        />
      );
    case 'changes':
      return (
        <ChangesWindow
          machineId={win.machineId ?? null}
          machineState={machineState}
          projectPath={win.projectId}
          events={events}
        />
      );
    case 'tasks':
      return (
        <TasksWindow
          machineId={win.machineId ?? null}
          machineState={machineState}
          projectPath={win.projectId}
        />
      );
    case 'logs':
      return <LogsWindow events={events} />;
    case 'settings':
      return <SettingsWindow />;
    case 'placeholder':
      return <Placeholder win={win} />;
    default:
      return null;
  }
}

function StoppedBody() {
  return (
    <div className="editor-banner" role="status">
      <p>Machine stopped. Start it to reconnect this window.</p>
    </div>
  );
}

// Placeholder is a dev-only window kind with a render counter, used by the 9.0
// skeleton test to prove that focus/move/min/max never remount window content.
function Placeholder({ win }: { win: WindowState }) {
  const renders = useRef(0);
  renders.current += 1;
  return (
    <div className="placeholder-body">
      <p>Placeholder: {win.title}</p>
      <p data-testid="render-count">renders: {renders.current}</p>
    </div>
  );
}

// useSurfaceSize measures the window surface itself (the area inside rail,
// context bar, and dock) rather than deriving it from window.innerWidth math,
// so maximize fills exactly the surface — never the rail/bars — and the dock
// appearing or disappearing is accounted for automatically. The size is also
// reported to the window manager so new windows cascade clamped inside it.
function useSurfaceSize(wm: ReturnType<typeof useWindowManager>): {
  surfaceRef: React.RefObject<HTMLDivElement | null>;
  viewport: { width: number; height: number };
} {
  const surfaceRef = useRef<HTMLDivElement | null>(null);
  // A sane pre-measure fallback: window minus rail (76) and context bar (44).
  const [viewport, setViewport] = useState(() => ({
    width: window.innerWidth - 76,
    height: window.innerHeight - 44,
  }));

  useEffect(() => {
    const el = surfaceRef.current;
    if (!el || typeof ResizeObserver === 'undefined') return;
    const measure = () => {
      const r = el.getBoundingClientRect();
      setViewport({ width: Math.round(r.width), height: Math.round(r.height) });
    };
    measure();
    const ro = new ResizeObserver(measure);
    ro.observe(el);
    return () => ro.disconnect();
  }, []);

  // Feed placement: the reducer no-ops when the size is unchanged, and wm's
  // identity changes on every window mutation, so depend on the size only.
  useEffect(() => {
    wm.setSurface(viewport);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [viewport.width, viewport.height]);

  return { surfaceRef, viewport };
}
