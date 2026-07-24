import { useState } from 'react';
import { Modal } from './Modal';

// PORT_MIN mirrors the control plane's default previewable range floor (1024
// terminal / 1025 code-server stay reserved). The server is authoritative — it
// 400s a reserved or out-of-range port even if an operator narrowed the range —
// so this is just a friendly client-side guard. Exported so every "expose a
// port" entry point (this dialog, the machine details panel) shares one range.
export const PORT_MIN = 1026;
export const PORT_MAX = 65535;

// OpenAppDialog asks the owner for a port their app listens on inside the active
// machine, then hands it to onOpen (which opens a preview window). It validates
// the range client-side for fast feedback; the preview window surfaces any
// server-side rejection.
export function OpenAppDialog({
  onClose,
  onOpen,
}: {
  onClose: () => void;
  onOpen: (port: number) => void;
}) {
  const [value, setValue] = useState('');
  const [err, setErr] = useState<string | null>(null);

  const submit = (e: React.FormEvent) => {
    e.preventDefault();
    const port = Number(value);
    if (!Number.isInteger(port) || port < PORT_MIN || port > PORT_MAX) {
      setErr(`Enter a port between ${PORT_MIN} and ${PORT_MAX}.`);
      return;
    }
    onOpen(port);
  };

  return (
    <Modal title="Open app on port" onClose={onClose}>
      <form className="open-app" onSubmit={submit}>
        <label className="field">
          <span className="field-label">Port</span>
          <input
            className="field-input"
            type="number"
            min={PORT_MIN}
            max={PORT_MAX}
            value={value}
            placeholder="e.g. 3000"
            autoFocus
            onChange={(e) => {
              setValue(e.target.value);
              setErr(null);
            }}
          />
          <span className="field-hint">
            A port your app is listening on inside the machine ({PORT_MIN}–{PORT_MAX}).
          </span>
        </label>

        {err && <div className="form-error">{err}</div>}

        <div className="modal-actions">
          <button type="button" className="btn-ghost" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="btn-primary">
            Open
          </button>
        </div>
      </form>
    </Modal>
  );
}
