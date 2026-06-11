import { useEffect } from "react";
import { Terminal } from "./Terminal";

// TerminalPanel is a modal overlay hosting a live terminal for a machine. It is
// opened from MachineCard when the machine is running; closing it disposes the
// terminal socket (windowing / multiple panels is Phase 9). Escape closes it.
export function TerminalPanel({ machineID, onClose }: { machineID: string; onClose: () => void }) {
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [onClose]);

  return (
    <div className="terminal-overlay" onMouseDown={onClose}>
      <div
        className="terminal-modal"
        role="dialog"
        aria-label="Machine terminal"
        onMouseDown={(e) => e.stopPropagation()}
      >
        <div className="terminal-modal-header">
          <span className="terminal-modal-title">Terminal</span>
          <button className="btn-ghost" onClick={onClose} aria-label="Close terminal">
            ✕
          </button>
        </div>
        <Terminal machineID={machineID} />
      </div>
    </div>
  );
}
