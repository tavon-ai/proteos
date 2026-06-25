// terminalSocket wraps the browser side of the terminal WebSocket protocol
// (guestwire): binary frames carry raw PTY bytes, text frames carry small JSON
// control messages. It owns reconnection with capped exponential backoff while
// the panel is open, surfaces a coarse status to the UI, and resets the
// terminal before each (re)connection so the guest's scrollback replay is
// idempotent rather than appended to stale content.
//
// Close-code handling mirrors the gateway (decision #11):
//   1000 normal       — the shell exited; terminal, no reconnect
//   4001 revoked       — session revoked / logged out; terminal, no reconnect
//   4002 machine_stop  — the machine stopped; terminal, no reconnect
//   anything else      — transient; reconnect with backoff
// The browser WebSocket API hides the pre-upgrade HTTP status (401/403/404/409),
// so a connection that never opens across several attempts is treated as a
// persistent rejection and we stop rather than hammer the gateway.

import { logger } from './logger';

export type TerminalStatus =
  | { kind: 'connecting' }
  | { kind: 'connected' }
  | { kind: 'reconnecting'; attempt: number }
  | { kind: 'closed'; reason: string };

export interface TerminalSocketHandlers {
  /** Raw PTY output to write into the terminal. */
  onData: (bytes: Uint8Array) => void;
  /** Reset the terminal before a scrollback replay (idempotent reconnects). */
  onReset: () => void;
  /** Coarse connection status for the UI banner. */
  onStatus: (status: TerminalStatus) => void;
}

export interface TerminalSocket {
  /** Send user input (keystrokes / paste) to the shell. */
  send: (data: string | Uint8Array<ArrayBuffer>) => void;
  /** Tell the guest the viewport changed. */
  resize: (cols: number, rows: number) => void;
  /** Close the socket and stop reconnecting. */
  dispose: () => void;
}

const BACKOFF_MIN_MS = 500;
const BACKOFF_MAX_MS = 8000;
const MAX_FAILED_OPENS = 5;

export function connectTerminal(url: string, handlers: TerminalSocketHandlers): TerminalSocket {
  // Log the path (machine/session params) but not the full URL, which on https
  // carries no secret yet keeps logs tidy.
  const log = logger.child({ component: 'terminal', path: new URL(url, location.href).pathname });
  let ws: WebSocket | null = null;
  let disposed = false;
  let backoff = BACKOFF_MIN_MS;
  let attempt = 0; // consecutive (re)connect attempts since the last open
  let everOpened = false;
  let reconnectTimer: ReturnType<typeof setTimeout> | undefined;

  function open() {
    handlers.onStatus(attempt === 0 ? { kind: 'connecting' } : { kind: 'reconnecting', attempt });
    log.debug(attempt === 0 ? 'connecting' : 'reconnecting', { attempt });

    const socket = new WebSocket(url);
    socket.binaryType = 'arraybuffer';
    ws = socket;

    socket.onopen = () => {
      everOpened = true;
      attempt = 0;
      backoff = BACKOFF_MIN_MS;
      // Reset before the guest replays its scrollback ring so reconnects don't
      // stack duplicate history in the viewport.
      handlers.onReset();
      handlers.onStatus({ kind: 'connected' });
      log.info('connected');
    };

    socket.onmessage = (ev) => {
      if (typeof ev.data === 'string') {
        // Control frame (hello/exit). hello needs no action; exit is followed by
        // a 1000 close handled in onclose.
        return;
      }
      handlers.onData(new Uint8Array(ev.data as ArrayBuffer));
    };

    socket.onclose = (ev) => {
      if (disposed) return;
      // Terminal close codes (no reconnect) are expected lifecycle, not errors.
      switch (ev.code) {
        case 1000:
          log.info('closed', { code: ev.code, reason: 'session_ended' });
          handlers.onStatus({ kind: 'closed', reason: 'Session ended.' });
          return;
        case 4001:
          log.info('closed', { code: ev.code, reason: 'revoked' });
          handlers.onStatus({ kind: 'closed', reason: 'Session revoked — please sign in again.' });
          return;
        case 4002:
          log.info('closed', { code: ev.code, reason: 'machine_stopped' });
          handlers.onStatus({ kind: 'closed', reason: 'Machine stopped.' });
          return;
        case 4003:
          log.info('closed', { code: ev.code, reason: 'provider_unavailable' });
          handlers.onStatus({
            kind: 'closed',
            reason: 'Provider unavailable — set its API key and try again.',
          });
          return;
      }
      attempt++;
      if (!everOpened && attempt > MAX_FAILED_OPENS) {
        log.error('giving up', { code: ev.code, attempt });
        handlers.onStatus({ kind: 'closed', reason: 'Unable to connect to the terminal.' });
        return;
      }
      log.warn('connection dropped', { code: ev.code, attempt });
      scheduleReconnect();
    };

    socket.onerror = () => {
      // onclose always follows; reconnection is handled there.
    };
  }

  function scheduleReconnect() {
    handlers.onStatus({ kind: 'reconnecting', attempt });
    reconnectTimer = setTimeout(open, backoff);
    backoff = Math.min(backoff * 2, BACKOFF_MAX_MS);
  }

  open();

  return {
    send(data) {
      if (!ws || ws.readyState !== WebSocket.OPEN) return;
      // PTY input MUST go as a binary frame: the guest reads binary frames as
      // raw PTY bytes and text frames as JSON control messages (resize), so a
      // string keystroke sent as text is parsed as JSON, fails, and is dropped.
      ws.send(typeof data === 'string' ? new TextEncoder().encode(data) : data);
    },
    resize(cols, rows) {
      if (ws && ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify({ type: 'resize', cols, rows }));
      }
    },
    dispose() {
      disposed = true;
      log.debug('dispose');
      if (reconnectTimer) clearTimeout(reconnectTimer);
      if (ws) {
        ws.onclose = null;
        ws.onerror = null;
        ws.close(1000);
      }
    },
  };
}

// terminalURL builds the same-origin gateway WebSocket URL. In dev, Vite proxies
// /gw to the control plane with ws:true. `session` is the opaque per-window id a
// reconnect resumes (Phase 9 decision #3); `cwd`, when set, scopes the shell to a
// project folder (/workspace/<repo>) — validated by the control plane.
export function terminalURL(machineID: string, session = 'main', cwd?: string): string {
  const proto = window.location.protocol === 'https:' ? 'wss' : 'ws';
  const params = new URLSearchParams({ machine: machineID, session });
  if (cwd) params.set('cwd', cwd);
  return `${proto}://${window.location.host}/gw/terminal?${params.toString()}`;
}

// agentURL builds the gateway WebSocket URL for a provider's agent session
// (/gw/agent/{provider}). The guest spawns the provider's injected launch
// command instead of a shell (Phase 5 decision #9). `session` is the opaque
// per-window id; `cwd` scopes the agent to a project folder (Phase 9 #3).
export function agentURL(
  machineID: string,
  provider: string,
  session?: string,
  cwd?: string,
): string {
  const proto = window.location.protocol === 'https:' ? 'wss' : 'ws';
  const params = new URLSearchParams({ machine: machineID });
  if (session) params.set('session', session);
  if (cwd) params.set('cwd', cwd);
  return `${proto}://${window.location.host}/gw/agent/${encodeURIComponent(provider)}?${params.toString()}`;
}
