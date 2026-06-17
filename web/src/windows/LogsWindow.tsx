import type { MachineEvent } from '../api/client';

// LogsWindow streams the machine_events SSE log (decision #7): the same rolling
// activity feed the retired MachineCard carried, now a first-class window. It is
// a pure view over the events the desktop already subscribes to.
export function LogsWindow({ events }: { events: MachineEvent[] }) {
  if (events.length === 0) {
    return <p className="muted logs-empty">No activity yet.</p>;
  }
  return (
    <ul className="logs-list">
      {events.map((e) => (
        <li key={e.id} className={`logs-row logs-${e.type}`}>
          <span className="event-time">{new Date(e.created_at).toLocaleTimeString()}</span>
          <span className="event-desc">{describe(e)}</span>
        </li>
      ))}
    </ul>
  );
}

function describe(e: MachineEvent): string {
  if (e.from_state && e.to_state) {
    const base = `${e.from_state} → ${e.to_state}`;
    return e.type === 'error' && reasonOf(e.payload) ? `${base}: ${reasonOf(e.payload)}` : base;
  }
  if (e.type === 'git.clone') {
    const ok = Boolean((e.payload as Record<string, unknown>).ok);
    const detail = String((e.payload as Record<string, unknown>).detail ?? '');
    return ok ? 'clone complete' : `clone failed${detail ? `: ${detail}` : ''}`;
  }
  return e.type;
}

function reasonOf(payload: Record<string, unknown>): string {
  const r = payload?.['reason'];
  return typeof r === 'string' ? r : '';
}
