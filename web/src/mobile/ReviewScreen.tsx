import { useEffect, useRef, useState } from 'react';
import {
  ApiError,
  type MachineSummary,
  type PRDetail,
  type PRState,
  type Project,
} from '../api/client';
import {
  useCommentPR,
  useMergePR,
  usePR,
  usePRChecks,
  usePRFiles,
  useProjects,
} from '../api/hooks';
import { parsePatch } from './diff';
import { BranchIcon, ChatIcon, CheckIcon, ChevronLeftIcon, CloseIcon } from './icons';

// ReviewScreen is the deep-link target: land on a PR, skim the files, read the
// diff, approve & merge — without leaving the phone.
export function ReviewScreen({
  repo,
  number,
  machineId,
  machine,
  avatarUrl,
  onBack,
}: {
  repo: string;
  number: number;
  machineId: string | null;
  machine: MachineSummary | null;
  avatarUrl: string;
  onBack: () => void;
}) {
  const hasNumber = number > 0;
  // The deep link's ?repo= query param is the fast path; when it's absent (the
  // plain /m/:machineId/pr/:number shape) fall back to resolving the repo from
  // the machine's cloned projects. This requires the machine to be running.
  const needsRepoLookup = hasNumber && !repo;
  const projects = useProjects(machineId, needsRepoLookup);
  const resolvedRepo = repo || repoFullNameFromProjects(projects.data?.projects) || '';
  const hasContext = !!resolvedRepo && hasNumber;

  const pr = usePR(resolvedRepo, number, hasContext);
  const files = usePRFiles(resolvedRepo, number, hasContext);
  const checks = usePRChecks(resolvedRepo, number, hasContext);
  const [selected, setSelected] = useState(0);
  const [commenting, setCommenting] = useState(false);

  if (!hasNumber) {
    return (
      <div className="m-screen">
        <div className="m-body m-centered">
          <p className="m-empty">
            No pull request selected. Open a review link to land here directly.
          </p>
        </div>
      </div>
    );
  }

  if (needsRepoLookup && !resolvedRepo) {
    if (projects.error) {
      return (
        <div className="m-screen">
          <div className="m-body m-centered">
            <p className="m-empty">{projectsErrorMessage(projects.error)}</p>
          </div>
        </div>
      );
    }
    return (
      <div className="m-screen">
        <div className="m-body m-centered">
          <span className="m-spinner" role="status" aria-label="Loading" />
        </div>
      </div>
    );
  }

  if (pr.isLoading) {
    return (
      <div className="m-screen">
        <div className="m-body m-centered">
          <span className="m-spinner" role="status" aria-label="Loading" />
        </div>
      </div>
    );
  }

  if (pr.error || !pr.data) {
    return (
      <div className="m-screen">
        <header className="m-header">
          <div className="m-header-top">
            <BackButton onBack={onBack} />
          </div>
        </header>
        <div className="m-body m-centered">
          <p className="m-empty">{prErrorMessage(pr.error)}</p>
        </div>
      </div>
    );
  }

  const detail = pr.data;
  const fileList = files.data?.files ?? [];
  const current = fileList[Math.min(selected, Math.max(fileList.length - 1, 0))];

  return (
    <div className="m-screen">
      <header className="m-header">
        <div className="m-header-top">
          <BackButton onBack={onBack} />
          {machine && (
            <span className="m-machine-chip">
              <span className={`m-chip-dot${machine.state === 'running' ? ' is-running' : ''}`} />
              {machine.name}
            </span>
          )}
          <span
            className="m-avatar"
            style={
              detail.author.avatar_url || avatarUrl
                ? { backgroundImage: `url(${detail.author.avatar_url || avatarUrl})` }
                : undefined
            }
          />
        </div>
        <div className="m-status-row">
          <StateChip state={detail.state} />
          <span className="m-pr-number">#{detail.number}</span>
        </div>
        <h1 className="m-pr-title">{detail.title}</h1>
        <div className="m-pr-repo">
          {resolvedRepo} · {detail.head}
        </div>
      </header>

      <div className="m-body">
        <div className="m-stat-strip">
          <span>
            {detail.changed_files} file{detail.changed_files === 1 ? '' : 's'}
          </span>
          <span className="m-added">+{detail.additions}</span>
          <span className="m-removed">−{detail.deletions}</span>
          <ChecksSummary
            checks={checks.data ?? null}
            loading={checks.isLoading}
            failed={!!checks.error}
          />
        </div>

        {files.isLoading && (
          <div className="m-centered m-file-loading">
            <span className="m-spinner" role="status" aria-label="Loading files" />
          </div>
        )}
        {files.error != null && <p className="m-empty">Could not load the changed files.</p>}
        {fileList.map((f, i) => (
          <button
            key={f.path}
            type="button"
            className={`m-file-row${i === selected ? ' is-selected' : ''}`}
            onClick={() => setSelected(i)}
          >
            <span className={`m-file-status m-file-status-${f.status}`}>{f.status}</span>
            <span className="m-file-path">{f.path}</span>
            {f.additions > 0 && <span className="m-added m-file-count">+{f.additions}</span>}
            {f.deletions > 0 && <span className="m-removed m-file-count">−{f.deletions}</span>}
          </button>
        ))}

        {current && <DiffView patch={current.patch} />}
      </div>

      <MergeBar
        detail={detail}
        repo={resolvedRepo}
        number={number}
        onComment={() => setCommenting(true)}
      />
      {commenting && (
        <CommentSheet repo={resolvedRepo} number={number} onClose={() => setCommenting(false)} />
      )}
    </div>
  );
}

function BackButton({ onBack }: { onBack: () => void }) {
  return (
    <button type="button" className="m-icon-btn m-back" aria-label="Back" onClick={onBack}>
      <ChevronLeftIcon size={24} />
    </button>
  );
}

// StateChip is the OPEN PR / DRAFT / MERGED / CLOSED pill.
function StateChip({ state }: { state: PRState }) {
  const label = state === 'open' ? 'OPEN PR' : state === 'draft' ? 'DRAFT' : state.toUpperCase();
  return (
    <span className={`m-state-chip m-state-${state}`}>
      <BranchIcon size={12} />
      {label}
    </span>
  );
}

function ChecksSummary({
  checks,
  loading,
  failed,
}: {
  checks: { total: number; passed: number; failed: number; pending: number } | null;
  loading: boolean;
  failed: boolean;
}) {
  if (loading || failed || !checks || checks.total === 0) return null;
  if (checks.failed > 0) {
    return <span className="m-checks m-checks-failed">✕ {checks.failed} failed</span>;
  }
  if (checks.pending > 0) {
    return <span className="m-checks">{checks.pending} pending</span>;
  }
  return (
    <span className="m-checks">
      <span className="m-checks-ok">
        <CheckIcon size={14} />
      </span>
      {checks.passed} check{checks.passed === 1 ? '' : 's'} passed
    </span>
  );
}

// DiffView renders the selected file's patch. Lines stay unwrapped; the block
// scrolls horizontally on its own so the page never does.
function DiffView({ patch }: { patch?: string }) {
  if (!patch) {
    return <p className="m-empty">No text preview for this file.</p>;
  }
  return (
    <div className="m-diff">
      {parsePatch(patch).map((line, i) => (
        <div key={i} className={`m-diff-line m-diff-${line.kind}`}>
          {line.kind === 'add' && '+'}
          {line.kind === 'del' && '−'}
          {line.text || ' '}
        </div>
      ))}
    </div>
  );
}

// MergeBar is the sticky action bar: the comment button and the one primary,
// thumb-reachable action. Merge is two-tap (tap → confirm within 4s → merge).
function MergeBar({
  detail,
  repo,
  number,
  onComment,
}: {
  detail: PRDetail;
  repo: string;
  number: number;
  onComment: () => void;
}) {
  const merge = useMergePR(repo, number);
  const [confirming, setConfirming] = useState(false);
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  useEffect(() => () => clearTimeout(timer.current), []);

  const tapMerge = () => {
    if (!confirming) {
      setConfirming(true);
      timer.current = setTimeout(() => setConfirming(false), 4000);
      return;
    }
    clearTimeout(timer.current);
    setConfirming(false);
    merge.mutate('merge');
  };

  const merged = detail.state === 'merged';
  const disabled = merged || detail.state === 'closed' || merge.isPending;

  return (
    <div className="m-action-bar-wrap">
      {merge.error != null && (
        <div className="m-error m-merge-error">{mergeErrorMessage(merge.error)}</div>
      )}
      <div className="m-action-bar">
        <button type="button" className="m-comment-btn" aria-label="Comment" onClick={onComment}>
          <ChatIcon size={20} />
        </button>
        <button
          type="button"
          className={`m-merge-btn${merged ? ' is-merged' : ''}${confirming ? ' is-confirming' : ''}`}
          disabled={disabled}
          onClick={tapMerge}
        >
          {merge.isPending ? (
            <>
              <span className="m-spinner m-spinner-light" /> Merging…
            </>
          ) : merged ? (
            <>
              <CheckIcon size={18} /> Merged
            </>
          ) : confirming ? (
            'Tap again to confirm'
          ) : (
            <>
              <CheckIcon size={18} /> Approve &amp; merge
            </>
          )}
        </button>
      </div>
    </div>
  );
}

// CommentSheet is the bottom sheet behind the chat-bubble button: a textarea +
// submit that posts a plain PR comment.
function CommentSheet({
  repo,
  number,
  onClose,
}: {
  repo: string;
  number: number;
  onClose: () => void;
}) {
  const comment = useCommentPR(repo, number);
  const [body, setBody] = useState('');

  const submit = (e: { preventDefault(): void }) => {
    e.preventDefault();
    if (!body.trim()) return;
    comment.mutate(body, { onSuccess: onClose });
  };

  return (
    <div className="m-sheet-backdrop" onClick={onClose}>
      <form
        className="m-comment-sheet"
        onClick={(e) => e.stopPropagation()}
        onSubmit={submit}
        aria-label="Add a comment"
      >
        <div className="m-sheet-handle-row">
          <span className="m-field-label">Comment</span>
          <button type="button" className="m-icon-btn" aria-label="Close" onClick={onClose}>
            <CloseIcon size={18} />
          </button>
        </div>
        <textarea
          className="m-textarea"
          value={body}
          rows={4}
          placeholder="Leave a comment on this pull request"
          autoFocus
          onChange={(e) => setBody(e.target.value)}
        />
        {comment.error != null && <div className="m-error">Could not post the comment.</div>}
        <button
          type="submit"
          className="m-primary-btn"
          disabled={comment.isPending || !body.trim()}
        >
          {comment.isPending ? 'Posting…' : 'Comment'}
        </button>
      </form>
    </div>
  );
}

// repoFullNameFromProjects derives an "owner/repo" full name from the first
// cloned project's git remote (https://github.com/owner/repo(.git) or
// git@github.com:owner/repo(.git)). Used when a deep link omits ?repo= and the
// repo must be resolved from the machine it names instead.
function repoFullNameFromProjects(projects: Project[] | undefined): string | null {
  const remote = projects?.[0]?.remote;
  if (!remote) return null;
  const match = remote.match(/github\.com[:/]([^/]+)\/([^/]+?)(?:\.git)?\/?$/);
  return match ? `${match[1]}/${match[2]}` : null;
}

function projectsErrorMessage(error: unknown): string {
  if (error instanceof ApiError && error.status === 409) {
    return 'Start the machine to load this pull request.';
  }
  return "Could not determine this pull request's repository.";
}

function prErrorMessage(error: unknown): string {
  if (error instanceof ApiError) {
    if (error.status === 404) return 'This pull request could not be found.';
    if (error.status === 409 && error.code === 'reconnect_github')
      return 'GitHub access needs to be reconnected. Open Settings on the desktop.';
  }
  return 'Could not load the pull request.';
}

function mergeErrorMessage(error: unknown): string {
  if (error instanceof ApiError) {
    switch (error.code) {
      case 'not_mergeable':
        return 'This pull request cannot be merged (conflicts, draft, or blocked checks).';
      case 'head_changed':
        return 'The branch changed while you were reviewing. Reload and try again.';
      case 'merge_forbidden':
        return 'You do not have permission to merge this pull request.';
      case 'reconnect_github':
        return 'GitHub access needs to be reconnected.';
    }
  }
  return 'Merge failed. Please try again.';
}
