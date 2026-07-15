import { useState } from 'react';
import { logsExportUrl, type LogEntry, type LogSource } from '../api/client';
import { useLogs } from '../api/hooks';

type SourceFilter = LogSource | 'all';

const TABS: { key: SourceFilter; label: string }[] = [
  { key: 'all', label: 'All' },
  { key: 'api', label: 'API' },
  { key: 'ui', label: 'UI' },
];

// AppLogsWindow is the Logs page (TAV-108): a live, filterable view over the
// control plane's captured Proteos application logs — its own API logs plus
// browser-reported UI errors/warnings. Firecracker/machine logs are a separate
// concern and never appear here (see the Activity window for machine events).
export function AppLogsWindow() {
  const [source, setSource] = useState<SourceFilter>('all');
  const { data, isLoading, isError } = useLogs(source);
  const entries = data?.entries ?? [];

  // Export downloads the currently filtered view as a plain-text attachment. A
  // programmatic <a download> click, matching the project-download pattern.
  const onExport = () => {
    const a = document.createElement('a');
    a.href = logsExportUrl(source === 'all' ? undefined : source);
    a.download = `proteos-logs-${source}.log`;
    document.body.appendChild(a);
    a.click();
    a.remove();
  };

  return (
    <div className="applogs-window">
      <div className="applogs-toolbar">
        <div className="applogs-tabs" role="tablist" aria-label="Log source">
          {TABS.map((t) => (
            <button
              key={t.key}
              role="tab"
              aria-selected={source === t.key}
              className={source === t.key ? 'applogs-tab active' : 'applogs-tab'}
              onClick={() => setSource(t.key)}
            >
              {t.label}
            </button>
          ))}
        </div>
        <button className="btn-secondary" onClick={onExport}>
          Export
        </button>
      </div>

      {isLoading && <p className="muted applogs-empty">Loading…</p>}
      {isError && <p className="error-inline applogs-empty">Could not load logs.</p>}
      {!isLoading && !isError && entries.length === 0 && (
        <p className="muted applogs-empty">No logs yet.</p>
      )}
      {!isLoading && !isError && entries.length > 0 && (
        <ul className="applogs-list">
          {entries
            .slice()
            .reverse()
            .map((e, i) => (
              <LogRow key={i} entry={e} />
            ))}
        </ul>
      )}
    </div>
  );
}

function LogRow({ entry }: { entry: LogEntry }) {
  const fields = entry.fields ?? {};
  const fieldEntries = Object.entries(fields);
  return (
    <li className={`applogs-row applogs-level-${entry.level.toLowerCase()}`}>
      <span className="applogs-time">{new Date(entry.time).toLocaleTimeString()}</span>
      <span className={`applogs-source applogs-source-${entry.source}`}>{entry.source}</span>
      <span className="applogs-level">{entry.level}</span>
      <span className="applogs-message">
        {entry.message}
        {fieldEntries.length > 0 && (
          <span className="applogs-fields">
            {fieldEntries.map(([k, v]) => (
              <span key={k} className="applogs-field">
                {k}={v}
              </span>
            ))}
          </span>
        )}
      </span>
    </li>
  );
}
