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
import { useSelectedMachine } from './selectedMachine';
import { useWindowManager } from './windowManagerContext';
import { openAgent, openEditor, openSettings, openTerminal } from './openers';

// ProjectsLauncher is the desktop's primary surface (decision #1): each cloned
// repo under /workspace is a tile with actions that open the editor, a terminal,
// or a coding agent *scoped to that repo's folder*. A "+ Clone repo" disclosure
// reveals the GitHub clone form (the Phase 7 ReposPanel logic). The unit of work
// is the project, so every window opened here is born pointed at one.
export function ProjectsLauncher({
  machineState,
  providers,
  events,
}: {
  machineState: MachineState;
  providers: Provider[];
  events: MachineEvent[];
}) {
  const { selectedId } = useSelectedMachine();
  const running = machineState === 'running';
  const { data, isLoading, error } = useProjects(selectedId, running);
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

      {showClone && <CloneForm events={events} />}

      {isLoading && <p className="muted">Loading projects…</p>}
      {error && <p className="muted">Could not load projects.</p>}
      {!isLoading && projects.length === 0 && (
        <p className="muted">
          No projects yet. Use <strong>+ Clone repo</strong> to clone one into your workspace.
        </p>
      )}

      <ul className="project-grid">
        {projects.map((p) => (
          <ProjectTile key={p.path} project={p} providers={providers} />
        ))}
      </ul>
    </div>
  );
}

function ProjectTile({ project, providers }: { project: Project; providers: Provider[] }) {
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
        <button className="btn-secondary" onClick={() => openEditor(wm, project)}>
          Editor
        </button>
        <button className="btn-secondary" onClick={() => openTerminal(wm, project)}>
          Terminal
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
                    openAgent(wm, project, p.key, p.display_name);
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

function CloneForm({ events }: { events: MachineEvent[] }) {
  const { selectedId } = useSelectedMachine();
  const { data, isLoading, error, refetch, isFetching } = useRepos();
  const clone = useCloneRepo(selectedId);
  const [clones, setClones] = useState<Record<string, CloneState>>({});

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

  const reconnect = reconnectRequired(error) || reconnectRequired(clone.error);

  return (
    <div className="clone-form">
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
  onClone,
}: {
  repo: Repo;
  clone: CloneState | undefined;
  pending: boolean;
  onClone: () => void;
}) {
  const cloning = clone?.status === 'cloning';
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
    </li>
  );
}
