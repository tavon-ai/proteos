import { useCallback, useEffect, useState } from 'react';
import { api, type MachineState } from '../api/client';

// EditorWindow hosts the machine's code-server editor inside a desktop window
// (Phase 9). It mints a one-shot web-session URL scoped to the window's project
// folder (decision #5) and points an iframe at it; the machine's editor subdomain
// validates the token, sets its partitioned cookie, and serves code-server opened
// on that folder. If the machine stops, a reconnect banner replaces the frame
// rather than leaving a dead editor.
//
// Unlike the retired Phase 8 EditorPanel, this is the window *body* only — the
// window chrome (title, close, drag) is provided by <Window>. The component is
// mounted once for the window's lifetime, so the iframe is never reloaded by a
// minimize/maximize/focus (decision #2).
export function EditorWindow({
  machineId,
  machineState,
  folder,
}: {
  machineId: string | null;
  machineState: MachineState;
  folder?: string;
}) {
  const [url, setUrl] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const running = machineState === 'running';

  const mint = useCallback(() => {
    if (!machineId) return;
    setError(null);
    api
      .webSession(machineId, folder)
      .then((s) => setUrl(s.url))
      .catch(() => setError('Could not open the editor. Try again.'));
  }, [machineId, folder]);

  // Mint once the machine is running (and re-mint if it returns to running after
  // a stop). The token is single-use and ≤60s, loaded immediately by the iframe.
  useEffect(() => {
    if (running && url === null) mint();
    if (!running) setUrl(null);
  }, [running, url, mint]);

  if (!running) {
    return (
      <div className="editor-banner" role="status">
        <p>Machine stopped. Start it to reopen the editor.</p>
      </div>
    );
  }
  if (error) {
    return (
      <div className="editor-banner" role="alert">
        <p>{error}</p>
        <button className="btn" onClick={mint}>
          Retry
        </button>
      </div>
    );
  }
  if (!url) {
    return (
      <div className="editor-banner" role="status">
        <p>Opening editor…</p>
      </div>
    );
  }
  return (
    <iframe
      className="editor-frame"
      src={url}
      title="code-server editor"
      allow="clipboard-read; clipboard-write"
    />
  );
}
