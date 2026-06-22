import { useMe, useUpdateUserPrefs } from '../api/hooks';

// DownloadsPanel lets the user choose what the project Download button includes
// when it zips a project. The choice is an account preference (download_as_is)
// stored server-side and applied by the /api/projects/download proxy, so it is
// consistent across devices and the button itself needs no per-click option.
export function DownloadsPanel() {
  const { data, isLoading } = useMe();
  const update = useUpdateUserPrefs();
  const asIs = data?.prefs.download_as_is ?? false;

  const choose = (downloadAsIs: boolean) => {
    if (downloadAsIs === asIs || update.isPending) return;
    update.mutate({ download_as_is: downloadAsIs });
  };

  return (
    <section className="downloads-panel">
      <h2>Project downloads</h2>
      <p className="muted">
        Choose what the <strong>Download</strong> button on a project includes when it zips your
        work.
      </p>

      {isLoading ? (
        <p className="muted">Loading…</p>
      ) : (
        <div className="download-options" role="radiogroup" aria-label="Download contents">
          <label className="download-option">
            <input
              type="radio"
              name="download-mode"
              checked={!asIs}
              disabled={update.isPending}
              onChange={() => choose(false)}
            />
            <span className="download-option-text">
              <strong>Clean export</strong>
              <span className="muted">
                {' '}
                — your files including uncommitted changes, excluding <code>.git</code> and anything
                in <code>.gitignore</code> (e.g. <code>node_modules</code>, build output).
              </span>
            </span>
          </label>

          <label className="download-option">
            <input
              type="radio"
              name="download-mode"
              checked={asIs}
              disabled={update.isPending}
              onChange={() => choose(true)}
            />
            <span className="download-option-text">
              <strong>Everything as-is</strong>
              <span className="muted">
                {' '}
                — the full project folder exactly as it is on disk, including <code>.git</code>{' '}
                history and ignored files.
              </span>
            </span>
          </label>
        </div>
      )}

      {update.isError && <span className="error-inline">Could not save preference.</span>}
    </section>
  );
}
