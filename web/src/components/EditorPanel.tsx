import { useCallback, useEffect, useState } from "react";
import { api, type MachineState } from "../api/client";

// EditorPanel is a modal overlay hosting the machine's code-server editor (Phase
// 8). On open it mints a one-shot web-session URL (POST /api/machine/web-session)
// and points an iframe at it; the machine's editor subdomain validates the token,
// sets its partitioned cookie, and serves code-server. The iframe is the default
// embed (decision #6: SameSite=None; Partitioned cookies carry into the
// cross-origin frame on current Chrome/Firefox/Safari); an "Open in new tab"
// fallback always works in a first-party context for any browser that blocks it.
//
// The panel is gated on the machine running: if it stops while open (SSE-driven
// state from the parent), a reconnect banner replaces the frame rather than
// leaving a dead editor.
export function EditorPanel({
  machineState,
  onClose,
}: {
  machineState: MachineState;
  onClose: () => void;
}) {
  const [url, setUrl] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const running = machineState === "running";

  const mint = useCallback(() => {
    setError(null);
    api
      .webSession()
      .then((s) => setUrl(s.url))
      .catch(() => setError("Could not open the editor. Try again."));
  }, []);

  // Escape closes the panel.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [onClose]);

  // Mint once the machine is running (and re-mint if it returns to running after
  // a stop). The token is single-use and ≤60s, loaded immediately by the iframe.
  useEffect(() => {
    if (running && url === null) mint();
    if (!running) setUrl(null);
  }, [running, url, mint]);

  return (
    <div className="terminal-overlay" onMouseDown={onClose}>
      <div
        className="terminal-modal editor-modal"
        role="dialog"
        aria-label="Editor"
        onMouseDown={(e) => e.stopPropagation()}
      >
        <div className="terminal-modal-header">
          <span className="terminal-modal-title">Editor</span>
          {url && running && (
            <a className="btn-ghost" href={url} target="_blank" rel="noopener noreferrer">
              Open in new tab ↗
            </a>
          )}
          <button className="btn-ghost" onClick={onClose} aria-label="Close editor">
            ✕
          </button>
        </div>

        {!running ? (
          <div className="editor-banner" role="status">
            <p>Machine stopped. Start it to reopen the editor.</p>
          </div>
        ) : error ? (
          <div className="editor-banner" role="alert">
            <p>{error}</p>
            <button className="btn" onClick={mint}>
              Retry
            </button>
          </div>
        ) : url ? (
          <iframe
            className="editor-frame"
            src={url}
            title="code-server editor"
            // Allow the editor to do what an editor needs (clipboard, downloads),
            // scoped to its own origin.
            allow="clipboard-read; clipboard-write"
          />
        ) : (
          <div className="editor-banner" role="status">
            <p>Opening editor…</p>
          </div>
        )}
      </div>
    </div>
  );
}
