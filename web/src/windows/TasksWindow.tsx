import { useState } from 'react';
import { ApiError, type AgentTask, type MachineState, type TaskEvent } from '../api/client';
import { useCreateTask, useProviders, useTaskEvents, useTasks } from '../api/hooks';

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
  const providers = useProviders();
  const [selected, setSelected] = useState<string | null>(null);
  const events = useTaskEvents(machineId, selected);

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
          <TaskStream events={events} task={rows.find((t) => t.id === selected)} />
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
  const [provider, setProvider] = useState(() => providers.find((p) => p.key === 'claude')?.key ?? providers[0]?.key ?? '');

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
        <button type="submit" className="btn-secondary" disabled={pending || !prompt.trim() || !provider}>
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
function TaskStream({ events, task }: { events: TaskEvent[]; task?: AgentTask }) {
  // Until the live stream produces a terminal frame, a finished task (selected
  // after the fact) still shows its stored summary via the task row.
  const terminal = events.find((e) => e.kind === 'result');
  return (
    <div className="tasks-events">
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
        <div className={`tasks-result${terminal.is_error ? ' is-error' : ''}`} role="status">
          {terminal.is_error ? '✗ ' : '✓ '}
          {terminal.text || (terminal.is_error ? terminal.error || 'failed' : 'done')}
          {typeof terminal.cost_usd === 'number' && terminal.cost_usd > 0 && (
            <span className="tasks-result-meta">
              {' '}
              · ${terminal.cost_usd.toFixed(4)}
              {terminal.num_turns ? ` · ${terminal.num_turns} turns` : ''}
            </span>
          )}
        </div>
      )}
    </div>
  );
}

// EventRow renders one normalized agent event by kind.
function EventRow({ ev }: { ev: TaskEvent }) {
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
      return null; // the terminal `result` is rendered by TaskStream
  }
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
    default:
      return err.detail || 'Could not start the task.';
  }
}
