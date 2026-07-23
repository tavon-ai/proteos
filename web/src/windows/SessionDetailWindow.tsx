import { sessionExportUrl, type AgentSession } from '../api/client';
import { useSession, useTaskEvents } from '../api/hooks';
import { EventRow, resultGlyph, resultText } from './agentEvents';

// SessionDetailWindow is the Sessions page's detail view (TAV-142): one coding
// agent session's full metadata plus its conversation, rendered the same way
// the per-project Tasks window renders a live run (EventRow) — reused here
// since it is the same normalized agent-event stream (AT2), just reached via
// the session's machine/id instead of the active project. A session whose
// agent-event ring buffer has already been reaped (finished more than a few
// minutes ago, see taskevents.Hub) still shows its stored result summary/
// error/usage — just not the turn-by-turn history, which is not persisted.
export function SessionDetailWindow({
  sessionId,
  onBack,
}: {
  sessionId: string | null;
  onBack: () => void;
}) {
  const { data: session, isLoading, isError } = useSession(sessionId);
  const events = useTaskEvents(session?.machine_id ?? null, session ? sessionId : null);

  const onExport = () => {
    if (!sessionId) return;
    const a = document.createElement('a');
    a.href = sessionExportUrl(sessionId);
    a.download = `proteos-session-${sessionId}.json`;
    document.body.appendChild(a);
    a.click();
    a.remove();
  };

  return (
    <div className="session-detail-window">
      <div className="session-detail-toolbar">
        <button className="btn-ghost" onClick={onBack}>
          ← Back to sessions
        </button>
        <button className="btn-secondary" onClick={onExport} disabled={!session}>
          Export
        </button>
      </div>

      {isLoading && <p className="muted session-detail-empty">Loading…</p>}
      {isError && <p className="error-inline session-detail-empty">Could not load this session.</p>}
      {!isLoading && !isError && session && <SessionBody session={session} events={events} />}
    </div>
  );
}

function SessionBody({
  session,
  events,
}: {
  session: AgentSession;
  events: ReturnType<typeof useTaskEvents>;
}) {
  const terminal = events.find((e) => e.kind === 'result');
  const finished =
    session.status === 'done' || session.status === 'failed' || session.status === 'canceled';

  return (
    <>
      <SessionMeta session={session} />
      <div className="tasks-events session-detail-events">
        {events.length === 0 && (
          <p className="muted session-detail-empty">
            {finished
              ? (session.result_summary ?? session.error) ||
                'No conversation detail available for this session.'
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

// SessionMeta renders the session's identifying/summary fields: where it ran,
// what provider, when, how long, and — when the agent reported it — cost.
// usage is a free-form JSON blob (controlplane/guestctl records cost_usd/
// num_turns/duration_ms today); any other key the agent adds shows up too
// rather than being silently dropped.
function SessionMeta({ session }: { session: AgentSession }) {
  const usage = session.usage ?? {};
  const durationMs =
    typeof usage.duration_ms === 'number' ? usage.duration_ms : durationFromTimestamps(session);
  const costUsd = typeof usage.cost_usd === 'number' ? usage.cost_usd : undefined;
  const extraUsage = Object.entries(usage).filter(([k]) => k !== 'duration_ms' && k !== 'cost_usd');

  return (
    <div className="session-detail-meta">
      <div className="session-detail-meta-head">
        <span className={`sessions-status sessions-${session.status.toLowerCase()}`}>
          {session.status}
        </span>
        <h2 className="session-detail-title">{session.project}</h2>
      </div>
      <dl className="session-detail-fields">
        <Field label="Machine">{session.machine_name}</Field>
        <Field label="Session ID">
          <span className="session-detail-id">{session.id}</span>
        </Field>
        <Field label="Provider">{session.provider}</Field>
        <Field label="Created">{fmtTime(session.created_at)}</Field>
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
      </dl>
      <p className="session-detail-prompt">{session.prompt}</p>
      {session.error && <p className="sessions-row-error">{session.error}</p>}
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

function durationFromTimestamps(session: AgentSession): number | undefined {
  if (!session.started_at) return undefined;
  const start = new Date(session.started_at).getTime();
  const end = session.ended_at ? new Date(session.ended_at).getTime() : Date.now();
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
