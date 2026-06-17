import { afterEach, describe, expect, it } from 'vitest';
import { logger, setLogLevel, setLogSink, getLogLevel, type LogRecord } from './logger';

// Each test captures records through a swapped sink and restores the previous
// sink + level afterward so the module's global state doesn't leak between tests.
function capture(): { records: LogRecord[]; restore: () => void } {
  const records: LogRecord[] = [];
  const prevSink = setLogSink((r) => records.push(r));
  const prevLevel = getLogLevel();
  return {
    records,
    restore: () => {
      setLogSink(prevSink);
      setLogLevel(prevLevel);
    },
  };
}

let active: { restore: () => void } | null = null;
afterEach(() => {
  active?.restore();
  active = null;
});

describe('logger', () => {
  it('emits level, message, and fields', () => {
    const cap = capture();
    active = cap;
    setLogLevel('debug');

    logger.info('hello', { a: 1 });

    expect(cap.records).toHaveLength(1);
    const r = cap.records[0];
    expect(r.level).toBe('info');
    expect(r.msg).toBe('hello');
    expect(r.fields).toEqual({ a: 1 });
    expect(r.time).toMatch(/^\d{4}-\d{2}-\d{2}T/); // ISO timestamp
  });

  it('drops records below the active threshold', () => {
    const cap = capture();
    active = cap;
    setLogLevel('warn');

    logger.debug('quiet');
    logger.info('quiet');
    logger.warn('loud');
    logger.error('loud');

    expect(cap.records.map((r) => r.level)).toEqual(['warn', 'error']);
  });

  it('child binds context onto every record, overridable per call', () => {
    const cap = capture();
    active = cap;
    setLogLevel('debug');

    const child = logger.child({ component: 'terminal', attempt: 0 });
    child.info('connecting');
    child.warn('dropped', { attempt: 3 });

    expect(cap.records[0].fields).toEqual({ component: 'terminal', attempt: 0 });
    // Per-call fields override bound fields of the same key.
    expect(cap.records[1].fields).toEqual({ component: 'terminal', attempt: 3 });
  });

  it('serializes Error values to message + type fields', () => {
    const cap = capture();
    active = cap;
    setLogLevel('debug');

    logger.error('boom', { err: new TypeError('bad input') });

    expect(cap.records[0].fields).toEqual({ err: 'bad input', err_type: 'TypeError' });
  });
});
