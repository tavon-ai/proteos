// @vitest-environment jsdom
//
// Unit tests for the browser side of the terminal WebSocket protocol: connect /
// reconnect-with-backoff, terminal close-code handling (1000/4001/4002/4003),
// the give-up path when the socket never opens, binary-vs-text frame semantics,
// and the URL builders. The WebSocket global is replaced with a scriptable fake
// so every network transition is driven synchronously from the test.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import {
  agentURL,
  connectTerminal,
  terminalURL,
  type TerminalStatus,
  type TerminalSocket,
} from './terminalSocket';

class FakeWebSocket {
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSING = 2;
  static readonly CLOSED = 3;

  static instances: FakeWebSocket[] = [];

  url: string;
  binaryType = 'blob';
  readyState: number = FakeWebSocket.CONNECTING;
  sent: (string | Uint8Array | ArrayBuffer)[] = [];
  closedWith: number | undefined;

  onopen: ((ev: Event) => void) | null = null;
  onmessage: ((ev: MessageEvent) => void) | null = null;
  onclose: ((ev: CloseEvent) => void) | null = null;
  onerror: ((ev: Event) => void) | null = null;

  constructor(url: string) {
    this.url = url;
    FakeWebSocket.instances.push(this);
  }

  send(data: string | Uint8Array | ArrayBuffer) {
    this.sent.push(data);
  }

  close(code?: number) {
    this.readyState = FakeWebSocket.CLOSED;
    this.closedWith = code;
  }

  // --- test drivers (a real socket fires these from the network) ------------
  serverOpen() {
    this.readyState = FakeWebSocket.OPEN;
    this.onopen?.(new Event('open'));
  }

  serverClose(code: number) {
    this.readyState = FakeWebSocket.CLOSED;
    this.onclose?.({ code } as CloseEvent);
  }

  serverMessage(data: string | ArrayBuffer) {
    this.onmessage?.({ data } as MessageEvent);
  }
}

function lastSocket(): FakeWebSocket {
  const ws = FakeWebSocket.instances[FakeWebSocket.instances.length - 1];
  if (!ws) throw new Error('no socket created');
  return ws;
}

function handlersSpy() {
  return {
    onData: vi.fn<(bytes: Uint8Array) => void>(),
    onReset: vi.fn<() => void>(),
    onStatus: vi.fn<(status: TerminalStatus) => void>(),
  };
}

function lastStatus(h: ReturnType<typeof handlersSpy>): TerminalStatus {
  const calls = h.onStatus.mock.calls;
  const call = calls[calls.length - 1];
  if (!call) throw new Error('onStatus never called');
  return call[0];
}

describe('connectTerminal', () => {
  let sock: TerminalSocket | undefined;

  beforeEach(() => {
    vi.useFakeTimers();
    FakeWebSocket.instances = [];
    vi.stubGlobal('WebSocket', FakeWebSocket);
  });

  afterEach(() => {
    sock?.dispose();
    sock = undefined;
    vi.unstubAllGlobals();
    vi.useRealTimers();
  });

  it('reports connecting, then resets and reports connected on open', () => {
    const h = handlersSpy();
    sock = connectTerminal('/gw/terminal?machine=m1&session=main', h);

    expect(lastStatus(h)).toEqual({ kind: 'connecting' });
    expect(lastSocket().binaryType).toBe('arraybuffer');
    expect(h.onReset).not.toHaveBeenCalled();

    lastSocket().serverOpen();
    expect(h.onReset).toHaveBeenCalledTimes(1);
    expect(lastStatus(h)).toEqual({ kind: 'connected' });
  });

  it('delivers binary frames as Uint8Array and ignores text control frames', () => {
    const h = handlersSpy();
    sock = connectTerminal('/gw/terminal?machine=m1', h);
    lastSocket().serverOpen();

    lastSocket().serverMessage(JSON.stringify({ type: 'hello' }));
    expect(h.onData).not.toHaveBeenCalled();

    lastSocket().serverMessage(new TextEncoder().encode('pty-bytes').buffer);
    expect(h.onData).toHaveBeenCalledTimes(1);
    const bytes = h.onData.mock.calls[0][0];
    expect(bytes).toBeInstanceOf(Uint8Array);
    expect(new TextDecoder().decode(bytes)).toBe('pty-bytes');
  });

  it.each([
    [1000, 'Session ended.'],
    [4001, 'Session revoked — please sign in again.'],
    [4002, 'Machine stopped.'],
    [4003, 'Provider unavailable — set its API key and try again.'],
  ])('treats close code %d as terminal and does not reconnect', (code, reason) => {
    const h = handlersSpy();
    sock = connectTerminal('/gw/terminal?machine=m1', h);
    lastSocket().serverOpen();
    lastSocket().serverClose(code);

    expect(lastStatus(h)).toEqual({ kind: 'closed', reason });
    vi.advanceTimersByTime(60_000);
    expect(FakeWebSocket.instances).toHaveLength(1);
  });

  it('reconnects after a transient drop with capped exponential backoff', () => {
    const h = handlersSpy();
    sock = connectTerminal('/gw/terminal?machine=m1', h);
    lastSocket().serverOpen();

    // Drop 1: reconnect scheduled at the 500ms floor.
    lastSocket().serverClose(1006);
    expect(lastStatus(h)).toEqual({ kind: 'reconnecting', attempt: 1 });
    vi.advanceTimersByTime(499);
    expect(FakeWebSocket.instances).toHaveLength(1);
    vi.advanceTimersByTime(1);
    expect(FakeWebSocket.instances).toHaveLength(2);

    // Drop 2 (still not reopened): backoff doubles to 1000ms.
    lastSocket().serverClose(1006);
    vi.advanceTimersByTime(999);
    expect(FakeWebSocket.instances).toHaveLength(2);
    vi.advanceTimersByTime(1);
    expect(FakeWebSocket.instances).toHaveLength(3);

    // Keep failing: the delay is capped at 8000ms (500·2^n stops growing).
    for (let i = 0; i < 6; i++) {
      lastSocket().serverClose(1006);
      vi.advanceTimersByTime(8000);
    }
    const before = FakeWebSocket.instances.length;
    lastSocket().serverClose(1006);
    vi.advanceTimersByTime(7999);
    expect(FakeWebSocket.instances).toHaveLength(before);
    vi.advanceTimersByTime(1);
    expect(FakeWebSocket.instances).toHaveLength(before + 1);
  });

  it('resets attempt counter and backoff once a reconnect succeeds', () => {
    const h = handlersSpy();
    sock = connectTerminal('/gw/terminal?machine=m1', h);
    lastSocket().serverOpen();

    lastSocket().serverClose(1006);
    vi.advanceTimersByTime(500);
    lastSocket().serverClose(1006); // backoff now 1000ms
    vi.advanceTimersByTime(1000);
    lastSocket().serverOpen(); // success resets attempt + backoff

    expect(lastStatus(h)).toEqual({ kind: 'connected' });
    lastSocket().serverClose(1006);
    expect(lastStatus(h)).toEqual({ kind: 'reconnecting', attempt: 1 });
    vi.advanceTimersByTime(500); // back at the floor, not 2000ms
    expect(FakeWebSocket.instances).toHaveLength(4);
  });

  it('gives up after MAX_FAILED_OPENS when the socket never opens', () => {
    const h = handlersSpy();
    sock = connectTerminal('/gw/terminal?machine=m1', h);

    // The pre-upgrade HTTP status is invisible to the browser API, so repeated
    // failed opens are treated as a persistent rejection: initial attempt + 5
    // retries, then stop.
    for (let i = 0; i < 5; i++) {
      lastSocket().serverClose(1006);
      vi.advanceTimersByTime(8000);
    }
    expect(FakeWebSocket.instances).toHaveLength(6);

    lastSocket().serverClose(1006);
    expect(lastStatus(h)).toEqual({ kind: 'closed', reason: 'Unable to connect to the terminal.' });
    vi.advanceTimersByTime(60_000);
    expect(FakeWebSocket.instances).toHaveLength(6);
  });

  it('sends keystrokes as binary frames and resize as a JSON text frame', () => {
    const h = handlersSpy();
    sock = connectTerminal('/gw/terminal?machine=m1', h);

    // Not open yet: input and resize are dropped, not queued.
    sock.send('early');
    sock.resize(80, 24);
    expect(lastSocket().sent).toHaveLength(0);

    lastSocket().serverOpen();
    sock.send('ls\n');
    sock.send(new TextEncoder().encode('raw'));
    sock.resize(120, 40);

    const sent = lastSocket().sent;
    expect(sent).toHaveLength(3);
    // A string keystroke MUST be encoded to bytes: a text frame would be parsed
    // by the guest as a JSON control message and dropped. (ArrayBuffer.isView
    // rather than instanceof: jsdom's TextEncoder returns a Uint8Array from
    // another realm.)
    expect(ArrayBuffer.isView(sent[0])).toBe(true);
    expect(new TextDecoder().decode(sent[0] as Uint8Array)).toBe('ls\n');
    expect(new TextDecoder().decode(sent[1] as Uint8Array)).toBe('raw');
    expect(sent[2]).toBe(JSON.stringify({ type: 'resize', cols: 120, rows: 40 }));
  });

  it('dispose closes with 1000 and cancels any pending reconnect', () => {
    const h = handlersSpy();
    sock = connectTerminal('/gw/terminal?machine=m1', h);
    lastSocket().serverOpen();
    lastSocket().serverClose(1006); // reconnect timer now pending

    sock.dispose();
    sock = undefined;
    expect(FakeWebSocket.instances[0].closedWith).toBe(1000);
    vi.advanceTimersByTime(60_000);
    expect(FakeWebSocket.instances).toHaveLength(1);
  });
});

describe('URL builders', () => {
  // The suite runs from jsdom's http:// base URL, so ws:// + its host is expected.
  const host = window.location.host;

  it('terminalURL defaults the session and omits cwd unless set', () => {
    expect(terminalURL('m-1')).toBe(`ws://${host}/gw/terminal?machine=m-1&session=main`);
    expect(terminalURL('m-1', 'win2', '/workspace/repo')).toBe(
      `ws://${host}/gw/terminal?machine=m-1&session=win2&cwd=%2Fworkspace%2Frepo`,
    );
  });

  it('agentURL escapes the provider and only sets given params', () => {
    expect(agentURL('m-1', 'claude')).toBe(`ws://${host}/gw/agent/claude?machine=m-1`);
    expect(agentURL('m-1', 'a/b', 's1', '/workspace/repo')).toBe(
      `ws://${host}/gw/agent/a%2Fb?machine=m-1&session=s1&cwd=%2Fworkspace%2Frepo`,
    );
  });
});
