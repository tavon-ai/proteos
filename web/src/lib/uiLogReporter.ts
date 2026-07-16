// installUILogReporter forwards warn/error log records to the control plane
// (POST /api/logs/ui) so they show up on the desktop's Logs page alongside the
// server's own logs (TAV-108). Debug/info stay browser-only — shipping the full
// volume would swamp the shared ring buffer without adding signal worth having.
//
// This is the "future build forward records to the control plane" this module
// was left for (see lib/logger.ts) — call it once at startup.
import { setLogSink, type LogRecord } from './logger';

// serializeFields flattens every field to a string: the wire shape POST
// /api/logs/ui accepts (and the server's own captured fields already are).
function serializeFields(fields: Record<string, unknown>): Record<string, string> {
  const out: Record<string, string> = {};
  for (const [k, v] of Object.entries(fields)) {
    out[k] = typeof v === 'string' ? v : JSON.stringify(v);
  }
  return out;
}

// report is a fire-and-forget POST that never throws and never logs through
// the `logger` module itself — doing so would recurse through this very sink.
function report(record: LogRecord): void {
  fetch('/api/logs/ui', {
    method: 'POST',
    credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json', 'X-Requested-By': 'proteos' },
    body: JSON.stringify({
      level: record.level,
      message: record.msg,
      fields: serializeFields(record.fields),
    }),
  }).catch(() => {
    // Best-effort: a dropped report is not itself an error worth surfacing.
  });
}

// installUILogReporter swaps in the reporting sink. Returns a restore function
// (mirrors setLogSink) mainly so tests can uninstall it.
export function installUILogReporter(): () => void {
  const prevSink = setLogSink((record) => {
    if (record.level === 'warn' || record.level === 'error') report(record);
  });
  return () => setLogSink(prevSink);
}
