// Structured logging for the web UI.
//
// The control plane logs structured JSON via Go's log/slog (level, msg, and
// key/value fields). This is the browser-side counterpart: every record is a
// level, a short stable message, and a bag of fields — never interpolated
// prose — so logs stay greppable and machine-parseable in the same shape as the
// server's. A component binds its own context once with logger.child({...}) and
// the fields ride along on every line.
//
// The default sink writes to the browser console (console.debug/info/warn/error
// by level), passing the human prefix and the structured record as separate
// arguments so devtools renders the fields as an expandable object. Tests and
// future log shippers can swap the sink with setLogSink.

export type LogLevel = 'debug' | 'info' | 'warn' | 'error';

// Fields is the key/value bag attached to a record. Error values are serialized
// to { message, type } by the sink so a thrown error logs usefully rather than
// as "[object Object]".
export type LogFields = Record<string, unknown>;

export interface LogRecord {
  time: string;
  level: LogLevel;
  msg: string;
  fields: LogFields;
}

export type LogSink = (record: LogRecord) => void;

// Ascending severity; a record is emitted only when its level is at or above the
// active threshold.
const LEVEL_ORDER: Record<LogLevel, number> = {
  debug: 10,
  info: 20,
  warn: 30,
  error: 40,
};

// envLevel reads VITE_LOG_LEVEL (e.g. "debug") when set, else defaults to debug
// in dev and info in production builds. import.meta.env is inlined by Vite.
function envLevel(): LogLevel {
  const raw = typeof import.meta !== 'undefined' ? import.meta.env?.VITE_LOG_LEVEL : undefined;
  if (raw === 'debug' || raw === 'info' || raw === 'warn' || raw === 'error') return raw;
  const dev = typeof import.meta !== 'undefined' && import.meta.env?.DEV;
  return dev ? 'debug' : 'info';
}

let threshold: LogLevel = envLevel();

// serializeFields turns Error values into a plain { message, type } shape so the
// sink (console or a shipper) never stringifies an Error to "[object Object]".
function serializeFields(fields: LogFields): LogFields {
  const out: LogFields = {};
  for (const [k, v] of Object.entries(fields)) {
    if (v instanceof Error) {
      out[k] = v.message;
      out[`${k}_type`] = v.name;
    } else {
      out[k] = v;
    }
  }
  return out;
}

const consoleMethod: Record<LogLevel, (...args: unknown[]) => void> = {
  debug: (...a) => console.debug(...a),
  info: (...a) => console.info(...a),
  warn: (...a) => console.warn(...a),
  error: (...a) => console.error(...a),
};

// consoleSink renders a compact prefix ("12:00:00.123 INFO terminal.connect")
// followed by the structured fields object, which devtools shows expandable.
function consoleSink(record: LogRecord): void {
  const prefix = `${record.time.slice(11, 23)} ${record.level.toUpperCase()} ${record.msg}`;
  const emit = consoleMethod[record.level];
  if (Object.keys(record.fields).length > 0) {
    emit(prefix, record.fields);
  } else {
    emit(prefix);
  }
}

let sink: LogSink = consoleSink;

// setLogLevel overrides the active threshold at runtime (e.g. from a dev toggle).
export function setLogLevel(level: LogLevel): void {
  threshold = level;
}

export function getLogLevel(): LogLevel {
  return threshold;
}

// setLogSink swaps the output target — tests capture records, and a future build
// could forward warn/error to the control plane. Returns the previous sink so
// callers can restore it.
export function setLogSink(next: LogSink): LogSink {
  const prev = sink;
  sink = next;
  return prev;
}

export interface Logger {
  debug(msg: string, fields?: LogFields): void;
  info(msg: string, fields?: LogFields): void;
  warn(msg: string, fields?: LogFields): void;
  error(msg: string, fields?: LogFields): void;
  // child binds extra context onto every record this logger emits; bound fields
  // are overridden by per-call fields of the same key.
  child(fields: LogFields): Logger;
}

function makeLogger(bound: LogFields): Logger {
  function emit(level: LogLevel, msg: string, fields?: LogFields): void {
    if (LEVEL_ORDER[level] < LEVEL_ORDER[threshold]) return;
    sink({
      time: new Date().toISOString(),
      level,
      msg,
      fields: serializeFields({ ...bound, ...fields }),
    });
  }
  return {
    debug: (msg, fields) => emit('debug', msg, fields),
    info: (msg, fields) => emit('info', msg, fields),
    warn: (msg, fields) => emit('warn', msg, fields),
    error: (msg, fields) => emit('error', msg, fields),
    child: (fields) => makeLogger({ ...bound, ...fields }),
  };
}

// logger is the root. Call logger.child({ component: 'terminal' }) to scope a
// module's lines, mirroring slog.With on the server.
export const logger: Logger = makeLogger({});
