import { useEffect, useState } from 'react';
import { ApiError, type GitFileStatus, type MachineEvent, type MachineState } from '../api/client';
import { useGitBranch, useGitCommit, useGitDiff, useGitPush, useGitStatus } from '../api/hooks';

interface PushState {
  opId: string;
  status: 'pushing' | 'done' | 'failed';
  detail?: string;
}

// ChangesWindow is the review surface (GR1): it shows what a coding agent (or the
// user) changed in a project's working tree before anything is committed — the
// per-file status plus a unified diff — fetched live over the control channel.
// It is read-only; committing/pushing are separate, explicitly-authorized actions
// (GR3+). Testing the running app happens through the existing port preview.
export function ChangesWindow({
  machineId,
  machineState,
  projectPath,
  events,
}: {
  machineId: string | null;
  machineState: MachineState;
  // Absolute /workspace path; the project name (the API parameter) is its
  // basename, since clones land at /workspace/<name>.
  projectPath?: string;
  events: MachineEvent[];
}) {
  const running = machineState === 'running';
  const project = basename(projectPath);
  const [staged, setStaged] = useState(false);
  const [showBranch, setShowBranch] = useState(false);
  // Files the user has unchecked for the next commit (default: all selected).
  const [deselected, setDeselected] = useState<Set<string>>(new Set());
  // The in-flight/last push, tracked by op_id against the git.push SSE event.
  const [push, setPush] = useState<PushState | null>(null);

  const status = useGitStatus(machineId, project, running);
  const diff = useGitDiff(machineId, project, staged, running);
  const pushMut = useGitPush(machineId, project);
  const branch = status.data?.branch;

  // Resolve an in-flight push when its git.push completion event arrives.
  useEffect(() => {
    if (!push || push.status !== 'pushing') return;
    for (const ev of events) {
      if (ev.type !== 'git.push') continue;
      const payload = ev.payload as Record<string, unknown>;
      if (String(payload.op_id ?? '') !== push.opId) continue;
      const ok = Boolean(payload.ok);
      setPush({
        opId: push.opId,
        status: ok ? 'done' : 'failed',
        detail: String(payload.detail ?? ''),
      });
    }
  }, [events, push]);

  const startPush = () => {
    if (!branch) return;
    pushMut.mutate(
      { branch, setUpstream: true },
      {
        onSuccess: (res) => setPush({ opId: res.op_id, status: 'pushing' }),
        onError: () => setPush({ opId: '', status: 'failed', detail: 'dispatch failed' }),
      },
    );
  };

  const toggle = (path: string) =>
    setDeselected((prev) => {
      const next = new Set(prev);
      if (next.has(path)) next.delete(path);
      else next.add(path);
      return next;
    });

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
  const selectedPaths = files.filter((f) => !deselected.has(f.path)).map((f) => f.path);

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
        <button className="btn-ghost" onClick={() => setShowBranch((s) => !s)}>
          {showBranch ? 'Close' : '+ Branch'}
        </button>
        <button
          className="btn-ghost"
          onClick={startPush}
          disabled={!branch || pushMut.isPending || push?.status === 'pushing'}
          title={branch ? `Push ${branch} to origin` : 'No branch'}
        >
          {push?.status === 'pushing' || pushMut.isPending ? 'Pushing…' : 'Push'}
        </button>
        <button
          className="btn-ghost"
          onClick={refresh}
          disabled={status.isFetching || diff.isFetching}
        >
          {status.isFetching || diff.isFetching ? 'Refreshing…' : 'Refresh'}
        </button>
      </div>

      {push && push.status !== 'pushing' && (
        <p
          className={push.status === 'done' ? 'muted changes-push-ok' : 'muted changes-push-err'}
          role="status"
        >
          {push.status === 'done'
            ? `Pushed ${branch ?? ''} to origin ✓`
            : `Push failed${push.detail ? `: ${push.detail}` : ''}`}
        </p>
      )}

      {showBranch && (
        <NewBranchForm
          machineId={machineId}
          project={project}
          onCreated={() => setShowBranch(false)}
        />
      )}

      {status.isError && <p className="muted changes-empty">Could not load status.</p>}
      {!status.isError && files.length === 0 && (
        <p className="muted changes-empty">No uncommitted changes.</p>
      )}
      {files.length > 0 && (
        <ul className="changes-files">
          {files.map((f) => (
            <li key={f.path} className="changes-file">
              <input
                type="checkbox"
                className="changes-select"
                checked={!deselected.has(f.path)}
                onChange={() => toggle(f.path)}
                aria-label={`Include ${f.path} in commit`}
              />
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

      {files.length > 0 && (
        <CommitForm
          machineId={machineId}
          project={project}
          selectedPaths={selectedPaths}
          allSelected={selectedPaths.length === files.length}
          onCommitted={() => setDeselected(new Set())}
        />
      )}
    </div>
  );
}

// CommitForm commits the selected changes (GR3) — the human review gate. It
// sends the explicit path list for a partial commit, or omits it (commit all)
// when every file is selected. On success the mutation invalidates status/diff/
// projects so the tree shows clean.
function CommitForm({
  machineId,
  project,
  selectedPaths,
  allSelected,
  onCommitted,
}: {
  machineId: string | null;
  project: string;
  selectedPaths: string[];
  allSelected: boolean;
  onCommitted: () => void;
}) {
  const [message, setMessage] = useState('');
  const commit = useGitCommit(machineId, project);

  const submit = () => {
    const msg = message.trim();
    if (!msg || selectedPaths.length === 0) return;
    commit.mutate(
      { message: msg, paths: allSelected ? undefined : selectedPaths },
      {
        onSuccess: () => {
          setMessage('');
          onCommitted();
        },
      },
    );
  };

  return (
    <div className="commit-form">
      <input
        type="text"
        className="commit-message-input"
        placeholder="Commit message"
        value={message}
        onChange={(e) => setMessage(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === 'Enter') submit();
        }}
      />
      <button
        className="btn-primary"
        disabled={!message.trim() || selectedPaths.length === 0 || commit.isPending}
        onClick={submit}
      >
        {commit.isPending ? 'Committing…' : `Commit ${selectedPaths.length}`}
      </button>
      {commit.error && <p className="muted commit-error">{commitErrorMessage(commit.error)}</p>}
    </div>
  );
}

// commitErrorMessage turns a commit failure into a short message.
function commitErrorMessage(error: unknown): string {
  if (error instanceof ApiError) {
    switch (error.code) {
      case 'empty_message':
        return 'Enter a commit message.';
      case 'nothing_to_commit':
        return 'Nothing selected to commit.';
      case 'machine_not_running':
        return 'Machine is not running.';
    }
  }
  return 'Could not commit.';
}

// NewBranchForm creates (and optionally switches to) a branch in the project
// (GR2). On success it closes itself; the status/diff/projects queries are
// invalidated by the mutation so the new current branch shows everywhere.
function NewBranchForm({
  machineId,
  project,
  onCreated,
}: {
  machineId: string | null;
  project: string;
  onCreated: () => void;
}) {
  const [name, setName] = useState('');
  const [checkout, setCheckout] = useState(true);
  const branch = useGitBranch(machineId, project);

  const submit = () => {
    const trimmed = name.trim();
    if (!trimmed) return;
    branch.mutate(
      { name: trimmed, checkout },
      {
        onSuccess: () => {
          setName('');
          onCreated();
        },
      },
    );
  };

  return (
    <div className="branch-form">
      <input
        type="text"
        className="branch-name-input"
        placeholder="feature/my-change"
        value={name}
        onChange={(e) => setName(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === 'Enter') submit();
        }}
      />
      <label className="branch-checkout">
        <input type="checkbox" checked={checkout} onChange={(e) => setCheckout(e.target.checked)} />
        Switch to it
      </label>
      <button
        className="btn-secondary"
        disabled={!name.trim() || branch.isPending}
        onClick={submit}
      >
        {branch.isPending ? 'Creating…' : 'Create'}
      </button>
      {branch.error && <p className="muted branch-error">{branchErrorMessage(branch.error)}</p>}
    </div>
  );
}

// branchErrorMessage turns a branch-create failure into a short message.
function branchErrorMessage(error: unknown): string {
  if (error instanceof ApiError) {
    switch (error.code) {
      case 'invalid_branch_name':
        return 'Invalid branch name.';
      case 'branch_exists':
        return 'A branch with that name already exists.';
      case 'machine_not_running':
        return 'Machine is not running.';
    }
  }
  return 'Could not create the branch.';
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
