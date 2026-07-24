import { taskExportUrl, type AgentTask } from '../api/client';
import { useTask, useTaskEvents } from '../api/hooks';
import { EventRow, resultGlyph, resultText } from './agentEvents';

// TaskDetailWindow is a machine's Tasks window's detail view: one headless
// agent task's full metadata plus its conversation, mirroring the Sessions
// page's detail view (TAV-142) but scoped to the task's own machine rather
// than reached globally. It reuses the same normalized agent-event stream
// (AT2) the live Tasks window renders (EventRow) — a task whose event ring
// buffer has already been reaped (finished more than a few minutes ago) still
// shows its stored result summary/error/usage, just not the turn-by-turn
// history, which is not persisted.
export function TaskDetailWindow({
  machineId,
  taskId,
  machineName,
  onBack,
  onOpenSession,
}: {
  machineId: string | null;
  taskId: string | null;
  machineName?: string;
  onBack: () => void;
  onOpenSession: (sessionId: string, project: string) => void;
}) {
  const { data: task, isLoading, isError } = useTask(machineId, taskId);
  const events = useTaskEvents(machineId, taskId);

  const onExport = () => {
    if (!machineId || !taskId) return;
    const a = document.createElement('a');
    a.href = taskExportUrl(machineId, taskId);
    a.download = `proteos-task-${taskId}.json`;
    document.body.appendChild(a);
    a.click();
    a.remove();
  };

  return (
    <div className="session-detail-window">
      <div className="session-detail-toolbar">
        <button className="btn-ghost" onClick={onBack}>
          ← Back to tasks
        </button>
        <button className="btn-secondary" onClick={onExport} disabled={!task}>
          Export
        </button>
      </div>

      {isLoading && <p className="muted session-detail-empty">Loading…</p>}
      {isError && <p className="error-inline session-detail-empty">Could not load this task.</p>}
      {!isLoading && !isError && task && (
        <TaskBody
          task={task}
          machineName={machineName}
          events={events}
          onOpenSession={onOpenSession}
        />
      )}
    </div>
  );
}

function TaskBody({
  task,
  machineName,
  events,
  onOpenSession,
}: {
  task: AgentTask;
  machineName?: string;
  events: ReturnType<typeof useTaskEvents>;
  onOpenSession: (sessionId: string, project: string) => void;
}) {
  const terminal = events.find((e) => e.kind === 'result');
  const finished = task.status === 'done' || task.status === 'failed' || task.status === 'canceled';

  return (
    <>
      <TaskMeta task={task} machineName={machineName} onOpenSession={onOpenSession} />
      <div className="tasks-events session-detail-events">
        {events.length === 0 && (
          <p className="muted session-detail-empty">
            {finished
              ? (task.result_summary ?? task.error) ||
                'No conversation detail available for this task.'
              : 'Waiting for the agent…'}
          </p>
        )}
        {events.map((ev, i) => (
          <EventRow key={i} ev={ev} />
        ))}
        {terminal && (
          <div
            className={`tasks-result${terminal.is_error && terminal.status !== 'canceled' ? ' is-error' : ''}`}
            role="status"
          >
            {resultGlyph(terminal)} {resultText(terminal)}
          </div>
        )}
      </div>
    </>
  );
}

// TaskMeta renders the task's identifying/summary fields: which machine it ran
// on, what provider, when, how long, and — when the agent reported it — cost.
// usage is a free-form JSON blob; any key beyond duration_ms/cost_usd/num_turns
// shows up too rather than being silently dropped.
function TaskMeta({
  task,
  machineName,
  onOpenSession,
}: {
  task: AgentTask;
  machineName?: string;
  onOpenSession: (sessionId: string, project: string) => void;
}) {
  const usage = task.usage ?? {};
  const durationMs =
    typeof usage.duration_ms === 'number' ? usage.duration_ms : durationFromTimestamps(task);
  const costUsd = typeof usage.cost_usd === 'number' ? usage.cost_usd : undefined;
  const extraUsage = Object.entries(usage).filter(([k]) => k !== 'duration_ms' && k !== 'cost_usd');

  return (
    <div className="session-detail-meta">
      <div className="session-detail-meta-head">
        <span className={`sessions-status sessions-${task.status.toLowerCase()}`}>
          {task.status}
        </span>
        <h2 className="session-detail-title">{task.project}</h2>
      </div>
      <dl className="session-detail-fields">
        <Field label="Machine">{machineName || '—'}</Field>
        <Field label="Task ID">
          <span className="session-detail-id">{task.id}</span>
        </Field>
        <Field label="Provider">{task.provider}</Field>
        <Field label="Created">{fmtTime(task.created_at)}</Field>
        {durationMs != null && <Field label="Duration">{fmtDuration(durationMs)}</Field>}
        {costUsd != null && <Field label="Cost">${costUsd.toFixed(4)}</Field>}
        {typeof usage.num_turns === 'number' && <Field label="Turns">{usage.num_turns}</Field>}
        {extraUsage
          .filter(([k]) => k !== 'num_turns')
          .map(([k, v]) => (
            <Field key={k} label={k.replace(/_/g, ' ')}>
              {String(v)}
            </Field>
          ))}
        {task.agent_session_id && (
          <Field label="Session">
            <button
              className="btn-ghost session-detail-session-link"
              onClick={() => onOpenSession(task.agent_session_id as string, task.project)}
            >
              View session →
            </button>
          </Field>
        )}
      </dl>
      <p className="session-detail-prompt">{task.prompt}</p>
      {task.error && <p className="sessions-row-error">{task.error}</p>}
    </div>
  );
}

function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="session-detail-field">
      <dt>{label}</dt>
      <dd>{children}</dd>
    </div>
  );
}

function durationFromTimestamps(task: AgentTask): number | undefined {
  if (!task.started_at) return undefined;
  const start = new Date(task.started_at).getTime();
  const end = task.ended_at ? new Date(task.ended_at).getTime() : Date.now();
  if (Number.isNaN(start) || Number.isNaN(end)) return undefined;
  return Math.max(0, end - start);
}

function fmtDuration(ms: number): string {
  const totalSeconds = Math.round(ms / 1000);
  if (totalSeconds < 60) return `${totalSeconds}s`;
  const minutes = Math.floor(totalSeconds / 60);
  const seconds = totalSeconds % 60;
  return `${minutes}m ${seconds}s`;
}

function fmtTime(iso: string): string {
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleString();
}
