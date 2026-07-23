import { useState } from 'react';
import { ApiError, type MachineSummary } from '../api/client';
import type { useMachineMutations } from '../api/hooks';
import { Modal } from './Modal';

type Step = 'confirm' | 'running' | 'blocked';

// DestroyMachineDialog is the guarded single-machine destroy flow (TAV-141):
// an explicit confirmation, then the machine's Claude coding-agent sessions
// are exported server-side before the machine is actually torn down. If that
// export fails (or is incomplete), the destroy is blocked and the dialog
// offers "Force delete anyway" to bypass it — same shape as DestroyAllDialog's
// guarded flow, scaled down to one machine.
//
// destroy is the caller's own useMachineMutations().destroy mutation (not a
// fresh one instantiated here) so ContextBar's busy/label state — which reads
// that same mutation's isPending — stays in sync with the destroy this dialog
// triggers.
export function DestroyMachineDialog({
  machine,
  destroy,
  onClose,
}: {
  machine: MachineSummary;
  destroy: ReturnType<typeof useMachineMutations>['destroy'];
  onClose: () => void;
}) {
  const [step, setStep] = useState<Step>('confirm');
  const [errorDetail, setErrorDetail] = useState<string | null>(null);

  const run = (force: boolean) => {
    setStep('running');
    setErrorDetail(null);
    destroy.mutate(
      { id: machine.id, force },
      {
        onSuccess: onClose,
        onError: (err) => {
          if (err instanceof ApiError && err.code === 'session_export_failed') {
            setErrorDetail(err.detail ?? 'Failed to export the machine’s Claude sessions.');
          } else {
            setErrorDetail('Could not destroy the machine. Please try again.');
          }
          setStep('blocked');
        },
      },
    );
  };

  // Block closing mid-run — same rationale as DestroyAllDialog.
  const handleClose = step === 'running' ? () => {} : onClose;

  return (
    <Modal title="Destroy machine" onClose={handleClose}>
      <div className="destroy-all">
        {step === 'confirm' && (
          <>
            <p className="destroy-all-warning">
              Destroy “{machine.name}”? Its Claude coding-agent sessions are exported first, then
              its persistent disk is wiped — this cannot be recovered.
            </p>
            <div className="modal-actions">
              <button type="button" className="btn-ghost" onClick={onClose}>
                Cancel
              </button>
              <button type="button" className="btn-danger" onClick={() => run(false)}>
                Yes, destroy
              </button>
            </div>
          </>
        )}

        {step === 'running' && <p className="destroy-all-progress">Exporting sessions…</p>}

        {step === 'blocked' && (
          <>
            <div className="form-error">{errorDetail}</div>
            <div className="modal-actions">
              <button type="button" className="btn-ghost" onClick={onClose}>
                Cancel
              </button>
              <button type="button" className="btn-danger" onClick={() => run(true)}>
                Force delete anyway
              </button>
            </div>
          </>
        )}
      </div>
    </Modal>
  );
}
