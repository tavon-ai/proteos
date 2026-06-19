import { useWindowManager } from './windowManagerContext';
import { useSelectedMachine } from './selectedMachine';

// Dock is the bottom strip listing every open window (decision #1). It lists
// windows from EVERY machine (not just the active one), so it doubles as a
// cross-machine switcher: clicking an item on another machine switches the
// active machine to it, then focuses the window (raising its z-order and
// restoring it if minimized). Machine-scoped items are prefixed with a short
// "M<n>:" tag so same-named projects on different machines stay distinguishable
// (e.g. "Terminal — a2" on two machines). Minimized windows render dimmed so the
// dock doubles as the "where did my window go?" answer.
export function Dock() {
  const wm = useWindowManager();
  const { machines, selectedId, setSelectedId } = useSelectedMachine();
  if (wm.windows.length === 0) return null;

  // Ordinal per machine (M1, M2, …) by position in the machines list, so the
  // prefix is stable and compact regardless of the machine's display name.
  const ordinal = new Map(machines.map((m, i) => [m.id, i + 1]));

  return (
    <div className="dock" role="toolbar" aria-label="Open windows">
      {wm.windows.map((w) => {
        const top = w.zIndex === wm.topZ && w.mode !== 'minimized';
        const tag = w.machineId ? ordinal.get(w.machineId) : undefined;
        const label = tag ? `M${tag}: ${w.title}` : w.title;
        return (
          <button
            key={w.id}
            className={
              'dock-item' +
              (w.mode === 'minimized' ? ' dock-item-min' : '') +
              (top ? ' dock-item-active' : '')
            }
            title={label}
            // Switch to the window's machine first (its desktop is otherwise
            // hidden), then focus — which un-minimizes and raises (see
            // desktopReducer "focus").
            onClick={() => {
              if (w.machineId && w.machineId !== selectedId) setSelectedId(w.machineId);
              wm.focus(w.id);
            }}
          >
            <span className={`dock-kind dock-kind-${w.kind}`} aria-hidden="true" />
            <span className="dock-label">{label}</span>
          </button>
        );
      })}
    </div>
  );
}
