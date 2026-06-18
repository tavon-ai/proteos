import { useEffect, useRef, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import type { MachineEvent, MachineState, MachineSummary, Me, Provider } from '../api/client';
import { useLogout, useMachineEvents, useMachines, useProviders } from '../api/hooks';
import { Terminal } from '../components/Terminal';
import { EditorWindow } from '../windows/EditorWindow';
import { LogsWindow } from '../windows/LogsWindow';
import { SettingsWindow } from '../windows/SettingsWindow';
import { Dock } from './Dock';
import { ProjectsLauncher } from './ProjectsLauncher';
import { SelectedMachineProvider, useSelectedMachine } from './selectedMachine';
import { Taskbar } from './Taskbar';
import { Window } from './Window';
import { WindowManagerProvider } from './WindowManager';
import { useWindowManager } from './windowManagerContext';
import { openProjects } from './openers';
import { useLayoutLoader, useLayoutSaver } from './useLayout';
import type { WindowState } from './windowState';

// Desktop is the product shell: a project-centric, multi-window desktop. It owns
// the live machines + event + provider subscriptions once (a single EventSource
// backs the whole UI) and provides the active-machine selection to the tree.
export function Desktop({ me }: { me: Me }) {
  const { data: machines } = useMachines(me.machines);
  const events = useMachineEvents();
  const { data: providers } = useProviders();

  return (
    <SelectedMachineProvider machines={machines ?? []}>
      <DesktopScoped me={me} events={events} providers={providers ?? []} />
    </SelectedMachineProvider>
  );
}

// DesktopScoped binds the window manager + layout persistence to the active
// machine. The WindowManagerProvider's onChange debounce-saves that machine's
// layout to its SQLite (decision #6).
function DesktopScoped({
  me,
  events,
  providers,
}: {
  me: Me;
  events: MachineEvent[];
  providers: Provider[];
}) {
  const { selected, selectedId } = useSelectedMachine();
  const running = selected?.state === 'running';
  const saveLayout = useLayoutSaver(selectedId, running);

  return (
    // Key by the active machine: switching machines fully remounts the window
    // manager, so the desktop starts empty and loads the selected machine's own
    // layout — windows from the previous machine can never linger.
    <WindowManagerProvider key={selectedId ?? 'none'} onChange={saveLayout}>
      <DesktopShell
        me={me}
        machine={selected}
        selectedId={selectedId}
        events={events}
        providers={providers}
      />
    </WindowManagerProvider>
  );
}

function DesktopShell({
  me,
  machine,
  selectedId,
  events,
  providers,
}: {
  me: Me;
  machine: MachineSummary | null;
  selectedId: string | null;
  events: MachineEvent[];
  providers: Provider[];
}) {
  const wm = useWindowManager();
  const navigate = useNavigate();
  const logout = useLogout();
  const running = machine?.state === 'running';
  const viewport = useViewport();

  // Scope the desktop to the active machine: reset on switch, restore its layout
  // once running (reconnects live PTYs by their opaque session ids).
  useLayoutLoader(wm, selectedId, running);

  // Open the Projects launcher on first paint so the desktop is never empty.
  const openedRef = useRef(false);
  useEffect(() => {
    if (openedRef.current) return;
    openedRef.current = true;
    openProjects(wm);
  }, [wm]);

  const onLogout = () => {
    logout.mutate(undefined, {
      onSettled: () => navigate('/login', { replace: true }),
    });
  };

  return (
    <div className="desktop">
      <Taskbar me={me} onLogout={onLogout} loggingOut={logout.isPending} />

      <div className="desktop-surface">
        {wm.windows.map((win) => (
          <Window key={win.id} win={win} viewport={viewport}>
            <WindowBody
              win={win}
              machine={machine}
              machineState={machine?.state ?? 'stopped'}
              events={events}
              providers={providers}
            />
          </Window>
        ))}
      </div>

      <Dock />
    </div>
  );
}

// WindowBody routes a window to its content component by kind. It is rendered
// inside <Window>, which mounts it once for the window's lifetime — so a live
// terminal or the editor iframe survives every minimize/maximize/focus.
function WindowBody({
  win,
  machine,
  machineState,
  events,
  providers,
}: {
  win: WindowState;
  machine: MachineSummary | null;
  machineState: MachineState;
  events: MachineEvent[];
  providers: Provider[];
}) {
  switch (win.kind) {
    case 'projects':
      return <ProjectsLauncher machineState={machineState} providers={providers} events={events} />;
    case 'terminal':
      return machine ? (
        <Terminal machineID={machine.id} session={win.session} cwd={win.cwd} />
      ) : (
        <StoppedBody />
      );
    case 'agent':
      return machine ? (
        <Terminal
          machineID={machine.id}
          provider={win.provider}
          session={win.session}
          cwd={win.cwd}
        />
      ) : (
        <StoppedBody />
      );
    case 'editor':
      return <EditorWindow machineState={machineState} folder={win.folder} />;
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

// useViewport tracks the desktop surface size for maximize. We use the window
// size minus the taskbar; the exact pixels matter little since maximize fills
// the surface via CSS bounds anyway.
function useViewport(): { width: number; height: number } {
  const [vp, setVp] = useState(() => ({
    width: window.innerWidth,
    height: window.innerHeight - 44,
  }));
  useEffect(() => {
    const onResize = () => setVp({ width: window.innerWidth, height: window.innerHeight - 44 });
    window.addEventListener('resize', onResize);
    return () => window.removeEventListener('resize', onResize);
  }, []);
  return vp;
}
