import { useState } from 'react';
import type { GitFileStatus, MachineState } from '../api/client';
import { useGitDiff, useGitStatus } from '../api/hooks';

// ChangesWindow is the review surface (GR1): it shows what a coding agent (or the
// user) changed in a project's working tree before anything is committed — the
// per-file status plus a unified diff — fetched live over the control channel.
// It is read-only; committing/pushing are separate, explicitly-authorized actions
// (GR3+). Testing the running app happens through the existing port preview.
export function ChangesWindow({
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
  const [staged, setStaged] = useState(false);

  const status = useGitStatus(machineId, project, running);
  const diff = useGitDiff(machineId, project, staged, running);

  const refresh = () => {
    status.refetch();
    diff.refetch();
  };

  if (!running) {
    return (
      <div className="editor-banner" role="status">
        <p>Machine stopped. Start it to review changes in {project || 'this project'}.</p>
      </div>
    );
  }
  if (!project) {
    return (
      <div className="editor-banner" role="alert">
        <p>No project selected for review.</p>
      </div>
    );
  }

  const files = status.data?.files ?? [];

  return (
    <div className="changes-window">
      <div className="changes-bar">
        <span className="changes-branch" title="Current branch">
          {status.data?.branch ?? '…'}
        </span>
        <div className="changes-toggle" role="tablist" aria-label="Diff view">
          <button
            role="tab"
            aria-selected={!staged}
            className={!staged ? 'btn-secondary' : 'btn-ghost'}
            onClick={() => setStaged(false)}
          >
            Working tree
          </button>
          <button
            role="tab"
            aria-selected={staged}
            className={staged ? 'btn-secondary' : 'btn-ghost'}
            onClick={() => setStaged(true)}
          >
            Staged
          </button>
        </div>
        <button
          className="btn-ghost"
          onClick={refresh}
          disabled={status.isFetching || diff.isFetching}
        >
          {status.isFetching || diff.isFetching ? 'Refreshing…' : 'Refresh'}
        </button>
      </div>

      {status.isError && <p className="muted changes-empty">Could not load status.</p>}
      {!status.isError && files.length === 0 && (
        <p className="muted changes-empty">No uncommitted changes.</p>
      )}
      {files.length > 0 && (
        <ul className="changes-files">
          {files.map((f) => (
            <li key={f.path} className="changes-file">
              <span className={`changes-code changes-code-${codeClass(f)}`} title={statusTitle(f)}>
                {codeBadge(f)}
              </span>
              <span className="changes-path" title={f.orig ? `${f.orig} → ${f.path}` : f.path}>
                {f.orig ? `${f.orig} → ${f.path}` : f.path}
              </span>
            </li>
          ))}
        </ul>
      )}

      <div className="changes-diff">
        {diff.isError ? (
          <p className="muted">Could not load diff.</p>
        ) : diff.data && diff.data.diff ? (
          <>
            {diff.data.truncated && (
              <p className="muted changes-truncated">
                Diff is too large to show in full — open the editor or a terminal to see the rest.
              </p>
            )}
            <pre className="changes-diff-body">{diff.data.diff}</pre>
          </>
        ) : (
          <p className="muted">{staged ? 'Nothing staged.' : 'No working-tree diff.'}</p>
        )}
      </div>
    </div>
  );
}

// basename returns the last path segment of an absolute /workspace path.
function basename(path?: string): string {
  if (!path) return '';
  const parts = path.split('/').filter(Boolean);
  return parts.length ? parts[parts.length - 1] : '';
}

// codeBadge condenses the two porcelain codes into a short label.
function codeBadge(f: GitFileStatus): string {
  if (f.index === '?' && f.worktree === '?') return 'U';
  // Prefer the staged code; fall back to the worktree code.
  const c = f.index !== ' ' && f.index !== '' ? f.index : f.worktree;
  return c || '·';
}

function codeClass(f: GitFileStatus): string {
  const c = codeBadge(f);
  switch (c) {
    case 'A':
      return 'added';
    case 'D':
      return 'deleted';
    case 'R':
      return 'renamed';
    case 'U':
      return 'untracked';
    default:
      return 'modified';
  }
}

function statusTitle(f: GitFileStatus): string {
  if (f.index === '?' && f.worktree === '?') return 'Untracked';
  const parts: string[] = [];
  if (f.index !== ' ' && f.index !== '') parts.push(`staged: ${f.index}`);
  if (f.worktree !== ' ' && f.worktree !== '') parts.push(`unstaged: ${f.worktree}`);
  return parts.join(', ') || 'changed';
}
