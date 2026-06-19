import { useCallback, useEffect, useState } from 'react';
import { api, type MachineState } from '../api/client';

// PreviewWindow frames a web app the owner is running on a port inside their
// machine (PP3). It mirrors EditorWindow: it mints a one-shot, ≤60s web-session
// URL — here on the m-<uuid>-p<port> preview origin — and points an iframe at it;
// the preview origin validates the token, sets its partitioned cookie, and
// proxies the in-VM app. If the machine stops, a reconnect banner replaces the
// frame rather than leaving a dead preview.
//
// Unlike the editor (code-server, which we control and allow to be framed), a
// user's app may bust out of frames or refuse embedding, so the window also
// offers "Open in new tab". The web-session token is single-use, so the new tab
// mints its own fresh URL rather than reusing the iframe's.
export function PreviewWindow({
  machineId,
  machineState,
  port,
}: {
  machineId: string | null;
  machineState: MachineState;
  port?: number;
}) {
  const [url, setUrl] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const running = machineState === 'running';

  const mint = useCallback(() => {
    if (!machineId || !port) return;
    setError(null);
    api
      .previewSession(machineId, port)
      .then((s) => setUrl(s.url))
      .catch(() => setError('Could not open the app. Try again.'));
  }, [machineId, port]);

  // Mint once the machine is running (and re-mint if it returns to running after
  // a stop). The token is single-use and ≤60s, loaded immediately by the iframe.
  useEffect(() => {
    if (running && url === null) mint();
    if (!running) setUrl(null);
  }, [running, url, mint]);

  // Open the app in a new browser tab with a freshly minted (single-use) token —
  // the robust path for an app that refuses to be framed.
  const openTab = useCallback(() => {
    if (!machineId || !port) return;
    api
      .previewSession(machineId, port)
      .then((s) => window.open(s.url, '_blank', 'noopener'))
      .catch(() => setError('Could not open the app. Try again.'));
  }, [machineId, port]);

  if (!port) {
    return (
      <div className="editor-banner" role="alert">
        <p>No port set for this preview.</p>
      </div>
    );
  }
  if (!running) {
    return (
      <div className="editor-banner" role="status">
        <p>Machine stopped. Start it to reopen the app on port {port}.</p>
      </div>
    );
  }

  return (
    <div className="preview-window">
      <div className="preview-bar">
        <span className="preview-port">Port {port}</span>
        <button className="btn-ghost" onClick={openTab}>
          Open in new tab ↗
        </button>
      </div>
      {error ? (
        <div className="editor-banner" role="alert">
          <p>{error}</p>
          <button className="btn" onClick={mint}>
            Retry
          </button>
        </div>
      ) : url ? (
        <iframe className="preview-frame" src={url} title={`App on port ${port}`} />
      ) : (
        <div className="editor-banner" role="status">
          <p>Opening app on port {port}…</p>
        </div>
      )}
    </div>
  );
}
