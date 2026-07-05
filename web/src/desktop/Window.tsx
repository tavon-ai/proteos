import { memo, type ReactNode } from 'react';
import { Rnd } from 'react-rnd';
import { useWindowManager } from './windowManagerContext';
import type { WindowState } from './windowState';

// Window is the chrome around one window's content: a draggable/resizable frame
// (react-rnd handles only the pointer math — decision #2) with our header
// (title, minimize, maximize, close). The content `children` are mounted ONCE
// and never unmounted until the window is closed: minimize is a CSS `display`
// toggle and maximize/restore is a geometry change, so neither remounts the
// content subtree (the editor iframe and live terminals must survive both).
//
// The component is memoized on the window object + children so a focus/raise of
// a *sibling* window (which bumps topZ but not this window) does not re-render
// this window's subtree.

const MIN_WIDTH = 280;
const MIN_HEIGHT = 180;

interface WindowProps {
  win: WindowState;
  children: ReactNode;
  /** The desktop viewport size, for maximize. */
  viewport: { width: number; height: number };
  /** Hidden (belongs to a non-active machine): kept mounted, display:none, so its
   *  live terminal/editor survives a machine switch. */
  hidden?: boolean;
}

function WindowImpl({ win, children, viewport, hidden }: WindowProps) {
  const wm = useWindowManager();
  const minimized = win.mode === 'minimized';
  const maximized = win.mode === 'maximized';

  return (
    <Rnd
      className="window"
      size={{ width: win.geometry.width, height: win.geometry.height }}
      position={{ x: win.geometry.x, y: win.geometry.y }}
      minWidth={MIN_WIDTH}
      minHeight={MIN_HEIGHT}
      bounds="parent"
      dragHandleClassName="window-header"
      disableDragging={maximized}
      enableResizing={!maximized}
      style={{
        zIndex: win.zIndex,
        display: minimized || hidden ? 'none' : 'flex',
      }}
      onMouseDownCapture={() => wm.focus(win.id)}
      onDragStop={(_e, d) => wm.move(win.id, d.x, d.y)}
      onResizeStop={(_e, _dir, ref, _delta, position) =>
        wm.resize(win.id, {
          x: position.x,
          y: position.y,
          width: ref.offsetWidth,
          height: ref.offsetHeight,
        })
      }
    >
      <div className="window-frame" data-kind={win.kind}>
        <div
          className="window-header"
          onDoubleClick={(e) => {
            // Ignore double-clicks on the control buttons (minimize/maximize/close);
            // only the title-bar surface itself toggles maximize, like other OSes.
            if ((e.target as HTMLElement).closest('.window-controls')) return;
            wm.toggleMaximize(win.id, viewport);
          }}
        >
          <span className={`dock-kind dock-kind-${win.kind}`} aria-hidden />
          <span className="window-title" title={win.title}>
            {win.title}
          </span>
          {/* Keep react-draggable from starting a drag on the buttons: a drag
              begun on a control swallows its click, so minimize/maximize/close
              would silently no-op. */}
          <div className="window-controls" onMouseDown={(e) => e.stopPropagation()}>
            <button
              className="window-btn"
              title="Minimize"
              aria-label="Minimize"
              onClick={() => wm.minimize(win.id)}
            >
              –
            </button>
            <button
              className="window-btn"
              title={maximized ? 'Restore' : 'Maximize'}
              aria-label={maximized ? 'Restore' : 'Maximize'}
              onClick={() => wm.toggleMaximize(win.id, viewport)}
            >
              {maximized ? '❐' : '▢'}
            </button>
            <button
              className="window-btn window-btn-close"
              title="Close"
              aria-label="Close"
              onClick={() => wm.close(win.id)}
            >
              ✕
            </button>
          </div>
        </div>
        <div className="window-body">{children}</div>
      </div>
    </Rnd>
  );
}

export const Window = memo(WindowImpl);
