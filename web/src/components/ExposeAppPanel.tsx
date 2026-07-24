import { useState } from 'react';
import { ApiError, api, type MachineState } from '../api/client';
import { PORT_MIN, PORT_MAX } from '../desktop/OpenAppDialog';
import { openPreview } from '../desktop/openers';
import { useWindowManager } from '../desktop/windowManagerContext';

interface ExposedApp {
  port: number;
  url: string;
}

// friendlyError maps the previewSession failure modes (see api/client.ts) to a
// message the owner can act on; anything else falls back to a generic retry.
function friendlyError(err: unknown): string {
  if (err instanceof ApiError) {
    if (err.status === 409) return 'Start the machine before exposing an app.';
    if (err.status === 400)
      return err.detail ?? `Enter a port between ${PORT_MIN} and ${PORT_MAX}.`;
  }
  return 'Could not open the app. Try again.';
}

// ExposeAppPanel is the machine-details entry point for Port Preview (PP3): the
// owner enters a port their app listens on inside the machine, mints a
// one-shot web-session URL for it (POST /api/machine/web-session?port=, the
// same mint OpenAppDialog/PreviewWindow use), and gets back a clickable,
// copyable URL plus a floating iframe preview (openPreview, reusing
// PreviewWindow). The list of exposed ports persists for as long as this
// panel stays open, so re-minting (the token is single-use and ≤60s) is one
// click away.
export function ExposeAppPanel({
  machineId,
  machineState,
}: {
  machineId: string;
  machineState: MachineState;
}) {
  const wm = useWindowManager();
  const running = machineState === 'running';

  const [value, setValue] = useState('');
  const [err, setErr] = useState<string | null>(null);
  const [minting, setMinting] = useState(false);
  const [exposed, setExposed] = useState<ExposedApp[]>([]);
  const [copiedPort, setCopiedPort] = useState<number | null>(null);

  const mint = (port: number) => {
    setMinting(true);
    setErr(null);
    api
      .previewSession(machineId, port)
      .then((s) => {
        setExposed((prev) => [{ port, url: s.url }, ...prev.filter((e) => e.port !== port)]);
        openPreview(wm, machineId, port);
        setValue('');
      })
      .catch((e) => setErr(friendlyError(e)))
      .finally(() => setMinting(false));
  };

  const submit = (e: React.FormEvent) => {
    e.preventDefault();
    const port = Number(value);
    if (!Number.isInteger(port) || port < PORT_MIN || port > PORT_MAX) {
      setErr(`Enter a port between ${PORT_MIN} and ${PORT_MAX}.`);
      return;
    }
    mint(port);
  };

  const copy = (app: ExposedApp) => {
    void navigator.clipboard?.writeText(app.url);
    setCopiedPort(app.port);
    setTimeout(() => setCopiedPort((p) => (p === app.port ? null : p)), 1500);
  };

  return (
    <section className="expose-app-panel">
      <h3>Expose app</h3>
      <p className="muted">
        Run an app inside this machine and reach it via a URL. Enter the port your app is listening
        on.
      </p>

      {!running ? (
        <p className="muted">Start the machine to expose an app.</p>
      ) : (
        <>
          <form className="expose-app-form" onSubmit={submit}>
            <input
              className="field-input"
              type="number"
              min={PORT_MIN}
              max={PORT_MAX}
              value={value}
              placeholder="e.g. 3000"
              onChange={(e) => {
                setValue(e.target.value);
                setErr(null);
              }}
            />
            <button type="submit" className="btn" disabled={minting || !value}>
              {minting ? 'Opening…' : 'Expose'}
            </button>
          </form>

          {err && <div className="form-error">{err}</div>}

          {exposed.length > 0 && (
            <ul className="expose-app-list">
              {exposed.map((app) => (
                <li key={app.port} className="expose-app-row">
                  <span className="expose-app-port">Port {app.port}</span>
                  <a
                    className="expose-app-url mono"
                    href={app.url}
                    target="_blank"
                    rel="noreferrer"
                  >
                    {app.url}
                  </a>
                  <div className="expose-app-row-actions">
                    <button type="button" className="btn-ghost" onClick={() => copy(app)}>
                      {copiedPort === app.port ? 'Copied' : 'Copy'}
                    </button>
                    <button
                      type="button"
                      className="btn-ghost"
                      onClick={() => openPreview(wm, machineId, app.port)}
                    >
                      Preview
                    </button>
                    <button
                      type="button"
                      className="btn-ghost"
                      onClick={() => setExposed((prev) => prev.filter((e) => e.port !== app.port))}
                    >
                      Remove
                    </button>
                  </div>
                </li>
              ))}
            </ul>
          )}
        </>
      )}
    </section>
  );
}
