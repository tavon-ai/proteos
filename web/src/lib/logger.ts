// Structured logging for the web UI, built on LogLayer (https://loglayer.dev).
//
// The control plane logs structured JSON via Go's log/slog (level, msg, and
// key/value fields). This is the browser-side counterpart: every record is a
// level, a short stable message, and a bag of fields — never interpolated prose
// — so logs stay greppable and machine-parseable in the same shape as the
// server's. A component binds its own context once with logger.child({...}) and
// the fields ride along on every line (LogLayer context); per-call fields
// (LogLayer metadata) override bound fields of the same key.
//
// We deliberately do NOT write to the console. LogLayer routes every record
// through a BlankTransport into the in-memory ring buffer below; recentLogs()
// exposes it for in-app debugging, and setLogSink() lets a future build forward
// records to the control plane (or let tests capture them). Nothing reaches
// console.* — swap the sink to change where logs go.

import { LogLayer, BlankTransport } from 'loglayer';

export type LogLevel = 'debug' | 'info' | 'warn' | 'error';

// Fields is the key/value bag attached to a record. Error values are flattened
// to <key> = message plus <key>_type = name (matching the control plane's habit
// of logging an `err` string) so a thrown error logs usefully rather than as an
// opaque object.
export type LogFields = Record<string, unknown>;

export interface LogRecord {
  time: string;
  level: LogLevel;
  msg: string;
  fields: LogFields;
}

export type LogSink = (record: LogRecord) => void;

// Ascending severity; a record is emitted only when its level is at or above the
// active threshold. Filtering here (rather than in the transport) keeps it cheap
// and deterministic — sub-threshold calls never touch LogLayer at all.
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

// serializeFields flattens Error values so neither the buffer nor a downstream
// shipper has to stringify an Error to "[object Object]".
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

// The default sink: a bounded in-memory ring buffer. No console output. Kept
// small so it is safe to retain for the life of the tab; recentLogs() returns a
// snapshot for an in-app log view or an error report.
const BUFFER_LIMIT = 500;
const buffer: LogRecord[] = [];

function bufferSink(record: LogRecord): void {
  buffer.push(record);
  if (buffer.length > BUFFER_LIMIT) buffer.shift();
}

// recentLogs returns a copy of the buffered records (oldest first).
export function recentLogs(): LogRecord[] {
  return buffer.slice();
}

let sink: LogSink = bufferSink;

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

// The LogLayer transport. shipToLogger receives the level, the message array
// (we always pass a single string), and `data` — LogLayer's merge of context +
// metadata (see _processLog: { ...context, ...metadata }, so per-call fields
// win). We turn that into a LogRecord and hand it to the active sink.
const transport = new BlankTransport({
  shipToLogger: ({ logLevel, messages, data, hasData }) => {
    sink({
      time: new Date().toISOString(),
      level: logLevel as LogLevel,
      msg: messages.join(' '),
      fields: hasData && data ? data : {},
    });
    return messages;
  },
});

// Minimal structural view of a LogLayer / ILogBuilder for dynamic level dispatch
// and metadata staging, without leaning on `any`.
interface Emitter {
  debug(msg: string): void;
  info(msg: string): void;
  warn(msg: string): void;
  error(msg: string): void;
  withMetadata(meta: LogFields): Emitter;
}

const root = new LogLayer({ transport });

export interface Logger {
  debug(msg: string, fields?: LogFields): void;
  info(msg: string, fields?: LogFields): void;
  warn(msg: string, fields?: LogFields): void;
  error(msg: string, fields?: LogFields): void;
  // child binds extra context onto every record this logger emits; bound fields
  // are overridden by per-call fields of the same key.
  child(fields: LogFields): Logger;
}

function wrap(ll: LogLayer): Logger {
  function emit(level: LogLevel, msg: string, fields?: LogFields): void {
    if (LEVEL_ORDER[level] < LEVEL_ORDER[threshold]) return;
    let entry = ll as unknown as Emitter;
    if (fields && Object.keys(fields).length > 0) {
      entry = entry.withMetadata(serializeFields(fields));
    }
    entry[level](msg);
  }
  return {
    debug: (msg, fields) => emit('debug', msg, fields),
    info: (msg, fields) => emit('info', msg, fields),
    warn: (msg, fields) => emit('warn', msg, fields),
    error: (msg, fields) => emit('error', msg, fields),
    // child() copies context; withContext binds this scope's persistent fields.
    child: (fields) => wrap(ll.child().withContext(serializeFields(fields))),
  };
}

// logger is the root. Call logger.child({ component: 'terminal' }) to scope a
// module's lines, mirroring slog.With on the server.
export const logger: Logger = wrap(root);
