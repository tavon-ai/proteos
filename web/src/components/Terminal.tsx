import { useEffect, useRef, useState } from "react";
import { Terminal as XTerm } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import "@xterm/xterm/css/xterm.css";

import {
  agentURL,
  connectTerminal,
  terminalURL,
  type TerminalSocket,
  type TerminalStatus,
} from "../lib/terminalSocket";

const RESIZE_DEBOUNCE_MS = 100;

// Terminal mounts an xterm.js instance bound to the gateway WebSocket for a
// machine. Output is written as raw bytes; input is sent as binary frames;
// viewport changes are fitted and a debounced resize control frame is sent. The
// underlying socket reconnects with backoff and resets the terminal before each
// scrollback replay (see terminalSocket).
//
// With a `provider`, it connects to that provider's agent session
// (/gw/agent/{provider}) instead of a plain shell.
export function Terminal({ machineID, provider }: { machineID: string; provider?: string }) {
  const containerRef = useRef<HTMLDivElement | null>(null);
  const [status, setStatus] = useState<TerminalStatus>({ kind: "connecting" });

  useEffect(() => {
    const container = containerRef.current;
    if (!container) return;

    const term = new XTerm({
      cursorBlink: true,
      fontFamily: 'ui-monospace, SFMono-Regular, Menlo, Consolas, "Liberation Mono", monospace',
      fontSize: 13,
      theme: { background: "#0b0e14" },
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

    const url = provider ? agentURL(machineID, provider) : terminalURL(machineID);
    socket = connectTerminal(url, {
      onData: (bytes) => term.write(bytes),
      onReset: () => term.reset(),
      onStatus: setStatus,
    });

    const inputSub = term.onData((data) => socket?.send(data));

    const observer = new ResizeObserver(fitAndResize);
    observer.observe(container);

    return () => {
      observer.disconnect();
      if (resizeTimer) clearTimeout(resizeTimer);
      inputSub.dispose();
      socket?.dispose();
      term.dispose();
    };
  }, [machineID, provider]);

  return (
    <div className="terminal-wrap">
      <StatusBanner status={status} />
      <div className="terminal-surface" ref={containerRef} />
    </div>
  );
}

function StatusBanner({ status }: { status: TerminalStatus }) {
  if (status.kind === "connected") return null;
  let text: string;
  switch (status.kind) {
    case "connecting":
      text = "Connecting…";
      break;
    case "reconnecting":
      text = `Reconnecting (attempt ${status.attempt})…`;
      break;
    case "closed":
      text = status.reason;
      break;
  }
  return (
    <div className={`terminal-banner terminal-banner-${status.kind}`} role="status">
      {text}
    </div>
  );
}
