import { useEffect, useState } from 'react';
import type { MachineEvent, MachineState, Project, Provider, Repo } from '../api/client';
import {
  reconnectRequired,
  useCloneRepo,
  useInvalidateProjects,
  useProjects,
  useRepos,
} from '../api/hooks';
import { GitHubStatus } from '../components/GitHubStatus';
import { useWindowManager } from './windowManagerContext';
import { openAgent, openChanges, openEditor, openSettings, openTerminal } from './openers';
import { parseRepoRef } from './repoRef';
import { looksLikeGrantFailure } from './cloneFailure';

// ProjectsLauncher is the desktop's primary surface (decision #1): each cloned
// repo under /workspace is a tile with actions that open the editor, a terminal,
// or a coding agent *scoped to that repo's folder*. A "+ Clone repo" disclosure
// reveals the GitHub clone form (the Phase 7 ReposPanel logic). The unit of work
// is the project, so every window opened here is born pointed at one.
export function ProjectsLauncher({
  machineId,
  machineState,
  providers,
  events,
}: {
  machineId: string | null;
  machineState: MachineState;
  providers: Provider[];
  events: MachineEvent[];
}) {
  const running = machineState === 'running';
  const { data, isLoading, error } = useProjects(machineId, running);
  const invalidateProjects = useInvalidateProjects();
  const [showClone, setShowClone] = useState(false);

  // A finished clone (git.clone event) refetches the project list so its tile
  // appears without a manual refresh (decision #4).
  useEffect(() => {
    if (events.some((e) => e.type === 'git.clone')) invalidateProjects();
  }, [events, invalidateProjects]);

  if (!running) {
    return <p className="muted launcher-empty">Start your machine to see and open projects.</p>;
  }

  const projects = data?.projects ?? [];

  return (
    <div className="launcher">
      <div className="launcher-head">
        <h2>Projects</h2>
        <button className="btn-secondary" onClick={() => setShowClone((s) => !s)}>
          {showClone ? 'Close' : '+ Clone repo'}
        </button>
      </div>

      {showClone && <CloneForm machineId={machineId} events={events} />}

      {isLoading && <p className="muted">Loading projects…</p>}
      {error && <p className="muted">Could not load projects.</p>}
      {!isLoading && projects.length === 0 && (
        <p className="muted">
          No projects yet. Use <strong>+ Clone repo</strong> to clone one into your workspace.
        </p>
      )}

      <ul className="project-grid">
        {projects.map((p) => (
          <ProjectTile key={p.path} machineId={machineId} project={p} providers={providers} />
        ))}
      </ul>
    </div>
  );
}

function ProjectTile({
  machineId,
  project,
  providers,
}: {
  machineId: string | null;
  project: Project;
  providers: Provider[];
}) {
  const wm = useWindowManager();
  const [agentMenu, setAgentMenu] = useState(false);
  const launchable = providers.filter((p) => p.enabled && p.key_set);
  const enabled = providers.filter((p) => p.enabled);

  return (
    <li className="project-tile">
      <div className="project-tile-head">
        <span className="project-name" title={project.path}>
          {project.name}
        </span>
        {project.branch && <span className="chip chip-branch">{project.branch}</span>}
        {project.dirty && (
          <span className="chip chip-dirty" title="Uncommitted changes">
            ●
          </span>
        )}
      </div>
      {project.last_commit_msg && (
        <p className="project-commit muted" title={project.last_commit_msg}>
          {project.last_commit_msg}
        </p>
      )}
      <div className="project-actions">
        <button
          className="btn-secondary"
          onClick={() => machineId && openEditor(wm, machineId, project)}
        >
          Editor
        </button>
        <button
          className="btn-secondary"
          onClick={() => machineId && openTerminal(wm, machineId, project)}
        >
          Terminal
        </button>
        <button
          className="btn-secondary"
          onClick={() => machineId && openChanges(wm, machineId, project)}
        >
          Changes
        </button>
        <div className="agent-menu-wrap">
          <button className="btn-primary" onClick={() => setAgentMenu((s) => !s)}>
            Agent ▾
          </button>
          {agentMenu && (
            <div className="agent-menu" role="menu">
              {launchable.length === 0 && (
                <button
                  className="agent-menu-item"
                  onClick={() => {
                    setAgentMenu(false);
                    openSettings(wm);
                  }}
                >
                  {enabled.length === 0
                    ? 'No providers configured — open Settings'
                    : 'Set an API key in Settings…'}
                </button>
              )}
              {launchable.map((p) => (
                <button
                  key={p.key}
                  className="agent-menu-item"
                  onClick={() => {
                    setAgentMenu(false);
                    if (machineId) openAgent(wm, machineId, project, p.key, p.display_name);
                  }}
                >
                  {p.display_name}
                </button>
              ))}
            </div>
          )}
        </div>
      </div>
    </li>
  );
}

// --- Clone form (Phase 7 ReposPanel logic, recomposed for the launcher) ------

type CloneStatus = 'cloning' | 'done' | 'failed';
interface CloneState {
  opId: string;
  status: CloneStatus;
  detail?: string;
}

function CloneForm({ machineId, events }: { machineId: string | null; events: MachineEvent[] }) {
  const { data, isLoading, error, refetch, isFetching } = useRepos();
  const clone = useCloneRepo(machineId);
  const [clones, setClones] = useState<Record<string, CloneState>>({});
  // Repos added by URL that aren't in the granted list. Kept newest-first so a
  // just-added repo shows at the top; deduped against the granted set on render.
  const [adHoc, setAdHoc] = useState<string[]>([]);
  const [urlInput, setUrlInput] = useState('');
  const [urlError, setUrlError] = useState(false);

  // Correlate git.clone completion events back to in-flight clones by op_id.
  useEffect(() => {
    setClones((prev) => {
      let changed = false;
      const next = { ...prev };
      for (const ev of events) {
        if (ev.type !== 'git.clone') continue;
        const opId = String((ev.payload as Record<string, unknown>).op_id ?? '');
        const ok = Boolean((ev.payload as Record<string, unknown>).ok);
        const detail = String((ev.payload as Record<string, unknown>).detail ?? '');
        for (const [fullName, st] of Object.entries(next)) {
          if (st.opId === opId && st.status === 'cloning') {
            next[fullName] = { opId, status: ok ? 'done' : 'failed', detail };
            changed = true;
          }
        }
      }
      return changed ? next : prev;
    });
  }, [events]);

  const onClone = (fullName: string) => {
    clone.mutate(fullName, {
      onSuccess: (res) =>
        setClones((prev) => ({ ...prev, [fullName]: { opId: res.op_id, status: 'cloning' } })),
    });
  };

  const onAddUrl = () => {
    const fullName = parseRepoRef(urlInput);
    if (!fullName) {
      setUrlError(true);
      return;
    }
    setUrlError(false);
    setUrlInput('');
    // Surface the row (deduped) even if it's already in the granted list, then
    // kick off the clone through the shared op_id state machine.
    setAdHoc((prev) => (prev.includes(fullName) ? prev : [fullName, ...prev]));
    onClone(fullName);
  };

  const reconnect = reconnectRequired(error) || reconnectRequired(clone.error);
  const granted = new Set((data?.repos ?? []).map((repo) => repo.full_name));
  const adHocRepos = adHoc.filter((name) => !granted.has(name));

  return (
    <div className="clone-form">
      <div className="clone-url">
        <input
          type="text"
          className="clone-url-input"
          placeholder="Add a GitHub repo by URL or owner/repo"
          value={urlInput}
          onChange={(e) => {
            setUrlInput(e.target.value);
            if (urlError) setUrlError(false);
          }}
          onKeyDown={(e) => {
            if (e.key === 'Enter') onAddUrl();
          }}
        />
        <button
          className="btn-secondary"
          disabled={!urlInput.trim() || clone.isPending}
          onClick={onAddUrl}
        >
          Add
        </button>
      </div>
      {urlError && (
        <p className="muted clone-url-error">
          Enter a GitHub repo as a URL (https://github.com/owner/repo) or owner/repo.
        </p>
      )}
      {adHocRepos.length > 0 && (
        <ul className="clone-list">
          {adHocRepos.map((fullName) => (
            <CloneRow
              key={fullName}
              repo={{ full_name: fullName, private: false, default_branch: '', pushed_at: '' }}
              clone={clones[fullName]}
              pending={clone.isPending}
              grantsUrl={data?.grants_url}
              onClone={() => onClone(fullName)}
            />
          ))}
        </ul>
      )}

      {reconnect && <GitHubStatus reconnect />}
      {isLoading && <p className="muted">Loading repositories…</p>}
      {error && !reconnect && (
        <p className="muted">
          Could not load repositories.{' '}
          <button className="btn-ghost" onClick={() => refetch()}>
            Retry
          </button>
        </p>
      )}
      {data && !reconnect && data.repos.length === 0 && (
        <p className="muted">
          ProteOS can&apos;t see any repositories yet.{' '}
          {data.grants_url && (
            <a href={data.grants_url} target="_blank" rel="noreferrer">
              Choose which repos ProteOS can access
            </a>
          )}
        </p>
      )}
      {data && !reconnect && data.repos.length > 0 && (
        <ul className="clone-list">
          {data.repos.map((repo) => (
            <CloneRow
              key={repo.full_name}
              repo={repo}
              clone={clones[repo.full_name]}
              pending={clone.isPending}
              grantsUrl={data?.grants_url}
              onClone={() => onClone(repo.full_name)}
            />
          ))}
        </ul>
      )}
      {data && !reconnect && (
        <p className="muted clone-footer">
          <button className="btn-ghost" onClick={() => refetch()} disabled={isFetching}>
            {isFetching ? 'Refreshing…' : 'Refresh'}
          </button>
        </p>
      )}
    </div>
  );
}

function CloneRow({
  repo,
  clone,
  pending,
  grantsUrl,
  onClone,
}: {
  repo: Repo;
  clone: CloneState | undefined;
  pending: boolean;
  grantsUrl?: string;
  onClone: () => void;
}) {
  const cloning = clone?.status === 'cloning';
  // An access failure means the repo is private and not shared with ProteOS (or
  // public and mistyped). Point the user at the grants page rather than the raw
  // git error, which is only the tooltip.
  const grantFailure = clone?.status === 'failed' && looksLikeGrantFailure(clone.detail ?? '');
  return (
    <li className="clone-row">
      <span className="repo-name">{repo.full_name}</span>
      {repo.private && <span className="badge badge-private">private</span>}
      <span className="clone-row-action">
        {clone?.status === 'done' && <span className="repo-cloned">Cloned ✓</span>}
        {clone?.status === 'failed' && (
          <span className="repo-failed" title={clone.detail}>
            Failed
          </span>
        )}
        <button className="btn-secondary" disabled={cloning || pending} onClick={onClone}>
          {cloning ? 'Cloning…' : clone?.status === 'done' ? 'Clone again' : 'Clone'}
        </button>
      </span>
      {grantFailure && (
        <p className="muted clone-hint">
          ProteOS can&apos;t access this repo. If it&apos;s private,{' '}
          {grantsUrl ? (
            <a href={grantsUrl} target="_blank" rel="noreferrer">
              share it with ProteOS
            </a>
          ) : (
            'share it with ProteOS'
          )}
          ; if it&apos;s public, check the name.
        </p>
      )}
    </li>
  );
}
