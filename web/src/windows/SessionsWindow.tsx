import { useState } from 'react';
import { sessionsExportUrl, type AgentSession, type SessionStatusFilter } from '../api/client';
import { useSessions } from '../api/hooks';

const TABS: { key: SessionStatusFilter; label: string }[] = [
  { key: 'all', label: 'All' },
  { key: 'active', label: 'Active' },
  { key: 'finished', label: 'Finished' },
];

// SessionsWindow is the Sessions page (TAV-107): a live, filterable view over
// the caller's coding agent sessions (headless task runs, AT1) across every
// machine they own — past and in-progress. Unlike the per-machine Tasks
// window, this is a global window (no project/machine selection needed) meant
// to answer "what is/was happening across all my machines" at a glance.
// onOpenDetail opens a session's detail view (TAV-142) when its row is clicked.
export function SessionsWindow({
  onOpenDetail,
}: {
  onOpenDetail: (session: AgentSession) => void;
}) {
  const [status, setStatus] = useState<SessionStatusFilter>('all');
  const { data, isLoading, isError } = useSessions(status);
  const sessions = data?.sessions ?? [];

  // Export downloads the currently filtered view as a CSV attachment. A
  // programmatic <a download> click, matching the Logs page's export pattern.
  const onExport = () => {
    const a = document.createElement('a');
    a.href = sessionsExportUrl(status);
    a.download = `proteos-sessions-${status}.csv`;
    document.body.appendChild(a);
    a.click();
    a.remove();
  };

  return (
    <div className="sessions-window">
      <div className="sessions-toolbar">
        <div className="sessions-tabs" role="tablist" aria-label="Session status">
          {TABS.map((t) => (
            <button
              key={t.key}
              role="tab"
              aria-selected={status === t.key}
              className={status === t.key ? 'sessions-tab active' : 'sessions-tab'}
              onClick={() => setStatus(t.key)}
            >
              {t.label}
            </button>
          ))}
        </div>
        <button className="btn-secondary" onClick={onExport}>
          Export
        </button>
      </div>

      {isLoading && <p className="muted sessions-empty">Loading…</p>}
      {isError && <p className="error-inline sessions-empty">Could not load sessions.</p>}
      {!isLoading && !isError && sessions.length === 0 && (
        <p className="muted sessions-empty">No sessions yet.</p>
      )}
      {!isLoading && !isError && sessions.length > 0 && (
        <ul className="sessions-list">
          {sessions.map((s) => (
            <SessionRow key={s.id} session={s} onOpen={() => onOpenDetail(s)} />
          ))}
        </ul>
      )}
    </div>
  );
}

function SessionRow({ session, onOpen }: { session: AgentSession; onOpen: () => void }) {
  return (
    <li className="sessions-row">
      <button className="sessions-row-btn" onClick={onOpen}>
        <div className="sessions-row-head">
          <span className={`sessions-status sessions-${session.status.toLowerCase()}`}>
            {session.status}
          </span>
          <span className="sessions-row-meta">
            {session.machine_name} · {session.project} · {session.provider} ·{' '}
            {fmtTime(session.created_at)}
          </span>
        </div>
        <p className="sessions-row-prompt">{session.prompt}</p>
        {session.result_summary && <p className="sessions-row-summary">{session.result_summary}</p>}
        {session.error && <p className="sessions-row-error">{session.error}</p>}
      </button>
    </li>
  );
}

function fmtTime(iso: string): string {
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleString();
}
