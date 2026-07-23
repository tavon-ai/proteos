import { useState } from 'react';
import { ApiError, type AgentTask, type MachineState, type TaskEvent } from '../api/client';
import {
  useCancelTask,
  useCreateTask,
  useProviders,
  useSendMessage,
  useTaskEvents,
  useTasks,
} from '../api/hooks';
import { EventRow, resultGlyph, resultText } from './agentEvents';

// TasksWindow is the headless task lane's live view (AT1/AT2): hand a project a
// natural-language task, watch the coding agent work in real time as structured
// events, and end with a reviewable dirty working tree (committed separately via
// the Changes window). The left column creates + lists this project's tasks; the
// right column streams the selected task's normalized agent events over SSE.
export function TasksWindow({
  machineId,
  machineState,
  projectPath,
}: {
  machineId: string | null;
  machineState: MachineState;
  // Absolute /workspace path; the project name (the API parameter) is its
  // basename, since clones land at /workspace/<name>.
  projectPath?: string;
}) {
  const running = machineState === 'running';
  const project = basename(projectPath);

  const tasks = useTasks(machineId, running);
  const createTask = useCreateTask(machineId);
  const cancelTask = useCancelTask(machineId);
  const sendMessage = useSendMessage(machineId);
  const providers = useProviders();
  const [selected, setSelected] = useState<string | null>(null);
  // Bumped on a follow-up turn so the event stream reconnects to the new turn.
  const [streamEpoch, setStreamEpoch] = useState(0);
  const events = useTaskEvents(machineId, selected, streamEpoch);

  // Providers usable on the headless lane from the browser's view: enabled with a
  // stored key. The server is the source of truth (it rejects a non-headless
  // provider with 400 provider_not_headless), but offering only key-set providers
  // avoids an obvious dead end.
  const usable = (providers.data ?? []).filter((p) => p.enabled && p.key_set);

  if (!running) {
    return (
      <div className="editor-banner" role="status">
        <p>Machine stopped. Start it to run tasks in {project || 'this project'}.</p>
      </div>
    );
  }
  if (!project) {
    return (
      <div className="editor-banner" role="alert">
        <p>No project selected for tasks.</p>
      </div>
    );
  }

  const rows = (tasks.data?.tasks ?? []).filter((t) => t.project === project);

  return (
    <div className="tasks-window">
      <div className="tasks-left">
        <TaskForm
          project={project}
          providers={usable.map((p) => ({ key: p.key, name: p.display_name }))}
          pending={createTask.isPending}
          error={createTask.error}
          onSubmit={(prompt, provider) =>
            createTask.mutate(
              { prompt, provider, project },
              { onSuccess: (res) => setSelected(res.task_id) },
            )
          }
        />
        <ul className="tasks-list">
          {rows.length === 0 && <li className="muted tasks-empty">No tasks yet.</li>}
          {rows.map((t) => (
            <li key={t.id}>
              <button
                className={`tasks-row${selected === t.id ? ' is-selected' : ''}`}
                onClick={() => setSelected(t.id)}
              >
                <span className={`tasks-status tasks-${t.status}`}>{t.status}</span>
                <span className="tasks-row-meta">
                  {t.provider} · {fmtTime(t.created_at)}
                </span>
                {t.result_summary && <span className="tasks-row-summary">{t.result_summary}</span>}
                {t.error && <span className="tasks-row-error">{t.error}</span>}
              </button>
            </li>
          ))}
        </ul>
      </div>
      <div className="tasks-stream">
        {selected ? (
          <TaskStream
            events={events}
            task={rows.find((t) => t.id === selected)}
            onCancel={() => selected && cancelTask.mutate(selected)}
            canceling={cancelTask.isPending}
            onFollowUp={(prompt) =>
              selected &&
              sendMessage.mutate(
                { taskId: selected, prompt },
                { onSuccess: () => setStreamEpoch((e) => e + 1) },
              )
            }
            sending={sendMessage.isPending}
            sendError={sendMessage.error}
          />
        ) : (
          <p className="muted tasks-stream-empty">Select a task to watch its live stream.</p>
        )}
      </div>
    </div>
  );
}

// TaskForm is the "dispatch a run" composer: a prompt plus a headless provider.
function TaskForm({
  project,
  providers,
  pending,
  error,
  onSubmit,
}: {
  project: string;
  providers: { key: string; name: string }[];
  pending: boolean;
  error: unknown;
  onSubmit: (prompt: string, provider: string) => void;
}) {
  const [prompt, setPrompt] = useState('');
  // Default to claude (the only headless provider) when present.
  const [provider, setProvider] = useState(
    () => providers.find((p) => p.key === 'claude')?.key ?? providers[0]?.key ?? '',
  );

  const submit = (e: React.FormEvent) => {
    e.preventDefault();
    const p = prompt.trim();
    if (!p || !provider) return;
    onSubmit(p, provider);
    setPrompt('');
  };

  return (
    <form className="tasks-form" onSubmit={submit}>
      <label className="tasks-form-label">
        Task for <strong>{project}</strong>
      </label>
      <textarea
        className="tasks-prompt"
        placeholder="Describe the change in plain language…"
        value={prompt}
        onChange={(e) => setPrompt(e.target.value)}
        rows={3}
      />
      <div className="tasks-form-row">
        {providers.length > 1 && (
          <select value={provider} onChange={(e) => setProvider(e.target.value)}>
            {providers.map((p) => (
              <option key={p.key} value={p.key}>
                {p.name}
              </option>
            ))}
          </select>
        )}
        <button
          type="submit"
          className="btn-secondary"
          disabled={pending || !prompt.trim() || !provider}
        >
          {pending ? 'Starting…' : 'Run task'}
        </button>
      </div>
      {providers.length === 0 && (
        <p className="muted tasks-form-hint">No provider key set — add one in Settings.</p>
      )}
      {error instanceof ApiError && (
        <p className="tasks-form-err" role="alert">
          {taskErrorMessage(error)}
        </p>
      )}
    </form>
  );
}

// TaskStream renders the normalized agent events as they arrive.
function TaskStream({
  events,
  task,
  onCancel,
  canceling,
  onFollowUp,
  sending,
  sendError,
}: {
  events: TaskEvent[];
  task?: AgentTask;
  onCancel: () => void;
  canceling: boolean;
  onFollowUp: (prompt: string) => void;
  sending: boolean;
  sendError: unknown;
}) {
  // Until the live stream produces a terminal frame, a finished task (selected
  // after the fact) still shows its stored summary via the task row.
  const terminal = events.find((e) => e.kind === 'result');
  const active = task && (task.status === 'running' || task.status === 'queued') && !terminal;
  // A finished task that captured a session can take a follow-up turn (AT4).
  const canFollowUp =
    !active &&
    !!task &&
    !!task.agent_session_id &&
    (task.status === 'done' || task.status === 'failed' || task.status === 'canceled');
  return (
    <div className="tasks-events">
      {active && (
        <div className="tasks-stream-bar">
          <span className="muted">Agent is working…</span>
          <button className="btn-ghost" onClick={onCancel} disabled={canceling}>
            {canceling ? 'Canceling…' : 'Cancel'}
          </button>
        </div>
      )}
      {events.length === 0 && (
        <p className="muted tasks-stream-empty">
          {task && (task.status === 'done' || task.status === 'failed')
            ? 'Connecting to the task’s event history…'
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
          {typeof terminal.cost_usd === 'number' && terminal.cost_usd > 0 && (
            <span className="tasks-result-meta">
              {' '}
              · ${terminal.cost_usd.toFixed(4)}
              {terminal.num_turns ? ` · ${terminal.num_turns} turns` : ''}
            </span>
          )}
        </div>
      )}
      {canFollowUp && <FollowUpForm onSubmit={onFollowUp} sending={sending} error={sendError} />}
    </div>
  );
}

// FollowUpForm sends a follow-up turn that resumes the agent session (AT4).
function FollowUpForm({
  onSubmit,
  sending,
  error,
}: {
  onSubmit: (prompt: string) => void;
  sending: boolean;
  error: unknown;
}) {
  const [prompt, setPrompt] = useState('');
  const submit = (e: React.FormEvent) => {
    e.preventDefault();
    const p = prompt.trim();
    if (!p) return;
    onSubmit(p);
    setPrompt('');
  };
  return (
    <form className="tasks-followup" onSubmit={submit}>
      <textarea
        className="tasks-prompt"
        placeholder="Follow up — e.g. “now also update the tests”…"
        value={prompt}
        onChange={(e) => setPrompt(e.target.value)}
        rows={2}
      />
      <div className="tasks-form-row">
        <button type="submit" className="btn-secondary" disabled={sending || !prompt.trim()}>
          {sending ? 'Sending…' : 'Send follow-up'}
        </button>
      </div>
      {error instanceof ApiError && (
        <p className="tasks-form-err" role="alert">
          {taskErrorMessage(error)}
        </p>
      )}
    </form>
  );
}

function basename(path?: string): string {
  if (!path) return '';
  const parts = path.split('/').filter(Boolean);
  return parts[parts.length - 1] ?? '';
}

function fmtTime(iso: string): string {
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? '' : d.toLocaleTimeString();
}

// taskErrorMessage maps a create-task ApiError code to a human message.
function taskErrorMessage(err: ApiError): string {
  switch (err.code) {
    case 'provider_not_headless':
      return 'That provider can’t run headless tasks (Claude Code only).';
    case 'no_provider_key':
      return 'No key set for that provider — add one in Settings.';
    case 'bad_project':
      return 'That project is not available on this machine.';
    case 'machine_not_running':
      return 'The machine is not running.';
    case 'no_session':
      return 'This task has no resumable session to continue.';
    case 'task_running':
      return 'A turn is already in progress — wait for it to finish.';
    default:
      return err.detail || 'Could not start the task.';
  }
}
