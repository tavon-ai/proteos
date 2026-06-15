import { useEffect, useState } from "react";
import type { Repo } from "../api/client";
import {
  reconnectRequired,
  useCloneRepo,
  useMachine,
  useMachineEvents,
  useRepos,
} from "../api/hooks";
import { GitHubStatus } from "./GitHubStatus";

type CloneStatus = "cloning" | "done" | "failed";
interface CloneState {
  opId: string;
  status: CloneStatus;
  detail?: string;
}

// ReposPanel lists the repositories the user has granted the GitHub App access
// to and lets them clone one into their machine's workspace (Phase 7 decision
// #8). Clone is async: the POST returns an op_id and completion arrives as a
// git.clone machine event over the SSE stream, which this panel correlates back
// to the originating button. The empty state and footer both link to the App's
// grant-management page, because with a GitHub App the user — not ProteOS —
// controls which repos are visible.
export function ReposPanel() {
  const { data, isLoading, error, refetch, isFetching } = useRepos();
  const clone = useCloneRepo();
  const events = useMachineEvents();
  const { data: machine } = useMachine(null);
  const running = machine?.state === "running";

  // Per-repo clone state, keyed by full_name.
  const [clones, setClones] = useState<Record<string, CloneState>>({});

  // Correlate git.clone completion events back to in-flight clones by op_id.
  useEffect(() => {
    setClones((prev) => {
      let changed = false;
      const next = { ...prev };
      for (const ev of events) {
        if (ev.type !== "git.clone") continue;
        const opId = String((ev.payload as Record<string, unknown>).op_id ?? "");
        const ok = Boolean((ev.payload as Record<string, unknown>).ok);
        const detail = String((ev.payload as Record<string, unknown>).detail ?? "");
        for (const [fullName, st] of Object.entries(next)) {
          if (st.opId === opId && st.status === "cloning") {
            next[fullName] = { opId, status: ok ? "done" : "failed", detail };
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
        setClones((prev) => ({ ...prev, [fullName]: { opId: res.op_id, status: "cloning" } })),
    });
  };

  const reconnect = reconnectRequired(error) || reconnectRequired(clone.error);

  return (
    <section className="repos-panel">
      <div className="repos-head">
        <h2>Repositories</h2>
        <GitHubStatus reconnect={reconnect} />
      </div>

      {!reconnect && (
        <p className="muted">
          Clone a repository into your machine&apos;s workspace. Commits and pushes
          use your GitHub identity; tokens are fetched on demand and never written
          to disk in the machine.
        </p>
      )}

      {isLoading && <p className="muted">Loading repositories…</p>}
      {error && !reconnect && (
        <p className="error-banner">
          Could not load repositories.{" "}
          <button className="btn-ghost" onClick={() => refetch()}>
            Retry
          </button>
        </p>
      )}

      {data && !reconnect && data.repos.length === 0 && (
        <p className="muted">
          ProteOS can&apos;t see any repositories yet.{" "}
          {data.grants_url && (
            <a href={data.grants_url} target="_blank" rel="noreferrer">
              Choose which repos ProteOS can access
            </a>
          )}
          .
        </p>
      )}

      {data && !reconnect && data.repos.length > 0 && (
        <>
          <ul className="repo-list">
            {data.repos.map((repo) => (
              <RepoRow
                key={repo.full_name}
                repo={repo}
                running={running}
                clone={clones[repo.full_name]}
                pending={clone.isPending}
                onClone={() => onClone(repo.full_name)}
              />
            ))}
          </ul>
          <p className="repos-footer muted">
            {!running && <span>Start your machine to clone. </span>}
            {data.grants_url && (
              <a href={data.grants_url} target="_blank" rel="noreferrer">
                Choose which repos ProteOS can access
              </a>
            )}
            {data.grants_url && " · "}
            <button className="btn-ghost" onClick={() => refetch()} disabled={isFetching}>
              {isFetching ? "Refreshing…" : "Refresh"}
            </button>
          </p>
        </>
      )}
    </section>
  );
}

function RepoRow({
  repo,
  running,
  clone,
  pending,
  onClone,
}: {
  repo: Repo;
  running: boolean;
  clone: CloneState | undefined;
  pending: boolean;
  onClone: () => void;
}) {
  const cloning = clone?.status === "cloning";
  return (
    <li className="repo-row">
      <div className="repo-meta">
        <span className="repo-name">{repo.full_name}</span>
        {repo.private && <span className="badge badge-private">private</span>}
        <span className="muted repo-pushed">updated {formatPushed(repo.pushed_at)}</span>
      </div>
      <div className="repo-action">
        {clone?.status === "done" && <span className="repo-cloned">Cloned ✓</span>}
        {clone?.status === "failed" && (
          <span className="repo-failed" title={clone.detail}>
            Clone failed
          </span>
        )}
        <button
          className="btn-secondary"
          disabled={!running || cloning || pending}
          title={!running ? "Start your machine first" : undefined}
          onClick={onClone}
        >
          {cloning ? "Cloning…" : clone?.status === "done" ? "Clone again" : "Clone"}
        </button>
      </div>
    </li>
  );
}

// formatPushed renders an RFC3339 timestamp as a short relative-ish label.
function formatPushed(rfc3339: string): string {
  const t = new Date(rfc3339);
  if (Number.isNaN(t.getTime())) return "unknown";
  const days = Math.floor((Date.now() - t.getTime()) / 86_400_000);
  if (days <= 0) return "today";
  if (days === 1) return "yesterday";
  if (days < 30) return `${days}d ago`;
  return t.toLocaleDateString();
}
