import { useEffect, useState } from 'react';
import { api, ApiError } from '../api/client';
import type { CloneTarget, MachineEvent, MachineState, Project, Repo } from '../api/client';
import {
  reconnectRequired,
  useCloneRepo,
  useInvalidateProjects,
  useProjects,
  useRepos,
} from '../api/hooks';
import { GitHubStatus } from '../components/GitHubStatus';
import { useWindowManager } from './windowManagerContext';
import { openChanges, openEditor, openTasks, openTerminal } from './openers';
import { parseCloneRef } from './repoRef';
import type { CloneRef } from './repoRef';
import { looksLikeGrantFailure } from './cloneFailure';

// ProjectsLauncher is the desktop's primary surface (decision #1): each cloned
// repo under /workspace is a tile with actions that open the editor, a terminal,
// or a coding agent *scoped to that repo's folder*. A "+ Clone repo" disclosure
// reveals the GitHub clone form (the Phase 7 ReposPanel logic). The unit of work
// is the project, so every window opened here is born pointed at one.
export function ProjectsLauncher({
  machineId,
  machineState,
  events,
}: {
  machineId: string | null;
  machineState: MachineState;
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
          <ProjectTile key={p.path} machineId={machineId} project={p} />
        ))}
      </ul>
    </div>
  );
}

function ProjectTile({ machineId, project }: { machineId: string | null; project: Project }) {
  const wm = useWindowManager();

  // Download the project as a zip of its current contents. A programmatic
  // <a download> click keeps styling identical to the other action buttons and
  // lets the browser stream the attachment to disk without navigating away.
  const onDownload = () => {
    if (!machineId) return;
    const a = document.createElement('a');
    a.href = api.projectDownloadUrl(machineId, project.path);
    a.download = `${project.name}.zip`;
    document.body.appendChild(a);
    a.click();
    a.remove();
  };

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
        <button
          className="btn-secondary"
          onClick={() => machineId && openTasks(wm, machineId, project)}
        >
          Tasks
        </button>
        <button className="btn-secondary" onClick={onDownload}>
          Download
        </button>
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
  // Clone rows are keyed by the ref's display label — "owner/repo" for GitHub,
  // "host/owner/repo" for clone-by-URL — so same-named repos on different
  // hosts stay distinct.
  const [clones, setClones] = useState<Record<string, CloneState>>({});
  // Repos added by URL that aren't in the granted list. Kept newest-first so a
  // just-added repo shows at the top; deduped against the granted set on render.
  const [adHoc, setAdHoc] = useState<CloneRef[]>([]);
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

  const onClone = (display: string, target: CloneTarget) => {
    clone.mutate(target, {
      onSuccess: (res) =>
        setClones((prev) => ({ ...prev, [display]: { opId: res.op_id, status: 'cloning' } })),
    });
  };

  const onAddUrl = () => {
    const ref = parseCloneRef(urlInput);
    if (!ref) {
      setUrlError(true);
      return;
    }
    setUrlError(false);
    setUrlInput('');
    // Surface the row (deduped) even if it's already in the granted list, then
    // kick off the clone through the shared op_id state machine.
    setAdHoc((prev) => (prev.some((r) => r.display === ref.display) ? prev : [ref, ...prev]));
    onClone(ref.display, ref.url ? { url: ref.url } : { full_name: ref.fullName as string });
  };

  const reconnect = reconnectRequired(error) || reconnectRequired(clone.error);
  // A clone-by-URL target whose host the server does not allow (or a URL the
  // server could not parse) rejects synchronously — surface it by the input.
  const hostRefused =
    clone.error instanceof ApiError &&
    (clone.error.code === 'forbidden_host' || clone.error.code === 'bad_url');
  const granted = new Set((data?.repos ?? []).map((repo) => repo.full_name));
  const adHocRepos = adHoc.filter((ref) => !granted.has(ref.display));

  return (
    <div className="clone-form">
      <div className="clone-url">
        <input
          type="text"
          className="clone-url-input"
          placeholder="Add a repo by URL (any allowed git host) or GitHub owner/repo"
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
          Enter a repo as a URL (https://host/owner/repo) or a GitHub owner/repo.
        </p>
      )}
      {hostRefused && (
        <p className="muted clone-url-error">
          This ProteOS server doesn&apos;t allow cloning from that git host. Ask your operator to
          add it to the allowed public hosts.
        </p>
      )}
      {adHocRepos.length > 0 && (
        <ul className="clone-list">
          {adHocRepos.map((ref) => (
            <CloneRow
              key={ref.display}
              repo={{ full_name: ref.display, private: false, default_branch: '', pushed_at: '' }}
              github={!ref.url}
              clone={clones[ref.display]}
              pending={clone.isPending}
              grantsUrl={data?.grants_url}
              onClone={() =>
                onClone(
                  ref.display,
                  ref.url ? { url: ref.url } : { full_name: ref.fullName as string },
                )
              }
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
              onClone={() => onClone(repo.full_name, { full_name: repo.full_name })}
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
  github = true,
  clone,
  pending,
  grantsUrl,
  onClone,
}: {
  repo: Repo;
  github?: boolean;
  clone: CloneState | undefined;
  pending: boolean;
  grantsUrl?: string;
  onClone: () => void;
}) {
  const cloning = clone?.status === 'cloning';
  // An access failure on a GitHub repo means it is private and not shared with
  // ProteOS (or public and mistyped) — point the user at the grants page rather
  // than the raw git error, which is only the tooltip. Public-host repos have
  // no grants flow: they are anonymous clone only, so the hint is different.
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
      {grantFailure && github && (
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
      {grantFailure && !github && (
        <p className="muted clone-hint">
          ProteOS can&apos;t access this repo. Only public repos can be cloned from this host —
          check that the repo exists and is public.
        </p>
      )}
    </li>
  );
}
