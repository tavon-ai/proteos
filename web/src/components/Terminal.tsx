import { useEffect, useRef, useState } from 'react';
import { Terminal as XTerm } from '@xterm/xterm';
import { FitAddon } from '@xterm/addon-fit';
import '@xterm/xterm/css/xterm.css';

import {
  agentURL,
  connectTerminal,
  terminalURL,
  type TerminalSocket,
  type TerminalStatus,
} from '../lib/terminalSocket';

const RESIZE_DEBOUNCE_MS = 100;

// Terminal mounts an xterm.js instance bound to the gateway WebSocket for a
// machine. Output is written as raw bytes; input is sent as binary frames;
// viewport changes are fitted and a debounced resize control frame is sent. The
// underlying socket reconnects with backoff and resets the terminal before each
// scrollback replay (see terminalSocket).
//
// With a `provider`, it connects to that provider's agent session
// (/gw/agent/{provider}) instead of a plain shell. `session` is the opaque
// per-window id a reconnect resumes; `cwd` scopes the shell/agent to a project
// folder (Phase 9 decision #3).
export function Terminal({
  machineID,
  provider,
  session,
  cwd,
}: {
  machineID: string;
  provider?: string;
  session?: string;
  cwd?: string;
}) {
  const containerRef = useRef<HTMLDivElement | null>(null);
  const [status, setStatus] = useState<TerminalStatus>({ kind: 'connecting' });

  useEffect(() => {
    const container = containerRef.current;
    if (!container) return;

    const term = new XTerm({
      cursorBlink: true,
      fontFamily: 'ui-monospace, SFMono-Regular, Menlo, Consolas, "Liberation Mono", monospace',
      fontSize: 13,
      theme: { background: '#0b0e14' },
      scrollback: 5000,
    });
    const fit = new FitAddon();
    term.loadAddon(fit);
    term.open(container);

    let socket: TerminalSocket | null = null;
    let resizeTimer: ReturnType<typeof setTimeout> | undefined;

    const fitAndResize = () => {
      try {
        fit.fit();
      } catch {
        // The container may be momentarily zero-sized (e.g. mid-transition).
        return;
      }
      if (resizeTimer) clearTimeout(resizeTimer);
      resizeTimer = setTimeout(() => socket?.resize(term.cols, term.rows), RESIZE_DEBOUNCE_MS);
    };

    // Fit once before connecting so the initial resize matches the viewport.
    try {
      fit.fit();
    } catch {
      /* ignore */
    }

    const url = provider
      ? agentURL(machineID, provider, session, cwd)
      : terminalURL(machineID, session ?? 'main', cwd);
    socket = connectTerminal(url, {
      onData: (bytes) => term.write(bytes),
      onReset: () => term.reset(),
      onStatus: setStatus,
    });

    const inputSub = term.onData((data) => socket?.send(data));

    // Shift+Enter inserts a literal newline (LF) rather than submitting the
    // line. xterm sends CR for Enter regardless of modifiers, so we intercept
    // the keydown and send LF instead — the convention CLI agents like Claude
    // Code use for multi-line input. Returning false suppresses xterm's default.
    term.attachCustomKeyEventHandler((event) => {
      if (event.type === 'keydown' && event.key === 'Enter' && event.shiftKey) {
        socket?.send('\n');
        return false;
      }
      return true;
    });

    // Copy-on-select: highlighting text with the mouse copies it to the
    // clipboard, like a native terminal emulator. mouseup is a user gesture, so
    // the Clipboard API is permitted; it may still be unavailable in an insecure
    // context, in which case we silently skip.
    const copySelection = () => {
      const selection = term.getSelection();
      if (selection) {
        void navigator.clipboard?.writeText(selection).catch(() => {
          /* clipboard unavailable or denied */
        });
      }
    };
    container.addEventListener('mouseup', copySelection);

    const observer = new ResizeObserver(fitAndResize);
    observer.observe(container);

    return () => {
      observer.disconnect();
      if (resizeTimer) clearTimeout(resizeTimer);
      container.removeEventListener('mouseup', copySelection);
      inputSub.dispose();
      socket?.dispose();
      term.dispose();
    };
  }, [machineID, provider, session, cwd]);

  return (
    <div className="terminal-wrap">
      <StatusBanner status={status} />
      <div className="terminal-surface" ref={containerRef} />
    </div>
  );
}

function StatusBanner({ status }: { status: TerminalStatus }) {
  if (status.kind === 'connected') return null;
  let text: string;
  switch (status.kind) {
    case 'connecting':
      text = 'Connecting…';
      break;
    case 'reconnecting':
      text = `Reconnecting (attempt ${status.attempt})…`;
      break;
    case 'closed':
      text = status.reason;
      break;
  }
  return (
    <div className={`terminal-banner terminal-banner-${status.kind}`} role="status">
      {text}
    </div>
  );
}
