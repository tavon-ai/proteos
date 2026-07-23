import type { TaskEvent } from '../api/client';

// agentEvents renders one normalized agent-event frame (AT2), shared by the
// per-project Tasks window and the Sessions detail view (TAV-142) — both
// stream the same task-events SSE, just reached via a different id (selected
// task vs. a session's machine_id + id). Kept in its own file (not
// TasksWindow.tsx) so both stay Fast-Refresh-friendly: a component file that
// also exports plain functions breaks React Fast Refresh.

// EventRow renders one normalized agent event by kind.
export function EventRow({ ev }: { ev: TaskEvent }) {
  switch (ev.kind) {
    case 'assistant_text':
      return <div className="tasks-ev tasks-ev-text">{ev.text}</div>;
    case 'tool_use':
      return (
        <div className="tasks-ev tasks-ev-tool">
          <span className="tasks-ev-toolname">{ev.tool || 'tool'}</span>
          {ev.input != null && <code className="tasks-ev-input">{summarizeInput(ev.input)}</code>}
        </div>
      );
    case 'tool_result':
      return (
        <div className={`tasks-ev tasks-ev-result${ev.is_error ? ' is-error' : ''}`}>
          <pre className="tasks-ev-output">{clip(ev.output ?? '')}</pre>
        </div>
      );
    default:
      return null; // the terminal `result` is rendered by the caller
  }
}

export function resultGlyph(ev: TaskEvent): string {
  if (ev.status === 'canceled') return '⊘';
  return ev.is_error ? '✗' : '✓';
}

export function resultText(ev: TaskEvent): string {
  if (ev.status === 'canceled') return 'Canceled';
  if (ev.text) return ev.text;
  return ev.is_error ? ev.error || 'failed' : 'done';
}

function summarizeInput(input: unknown): string {
  try {
    const s = typeof input === 'string' ? input : JSON.stringify(input);
    return clip(s, 200);
  } catch {
    return '';
  }
}

function clip(s: string, max = 2000): string {
  return s.length > max ? s.slice(0, max) + '…' : s;
}
