import { useWindowManager } from "./WindowManager";

// Dock is the bottom strip listing every open window (decision #1). Clicking an
// item focuses it (raising its z-order) and restores it if minimized — the
// tmux-of-windows quick-switch. Minimized windows render dimmed so the dock
// doubles as the "where did my window go?" answer.
export function Dock() {
  const wm = useWindowManager();
  if (wm.windows.length === 0) return null;
  return (
    <div className="dock" role="toolbar" aria-label="Open windows">
      {wm.windows.map((w) => {
        const top = w.zIndex === wm.topZ && w.mode !== "minimized";
        return (
          <button
            key={w.id}
            className={
              "dock-item" +
              (w.mode === "minimized" ? " dock-item-min" : "") +
              (top ? " dock-item-active" : "")
            }
            title={w.title}
            // focus both un-minimizes and raises (see desktopReducer "focus").
            onClick={() => wm.focus(w.id)}
          >
            <span className={`dock-kind dock-kind-${w.kind}`} aria-hidden="true" />
            <span className="dock-label">{w.title}</span>
          </button>
        );
      })}
    </div>
  );
}
