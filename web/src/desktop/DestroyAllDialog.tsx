import { useRef, useState } from 'react';
import type { DestroyAllResponse, MachineSummary } from '../api/client';
import { useDestroyAllMachines } from '../api/hooks';
import { Modal } from './Modal';

type Step = 'confirm' | 'running' | 'done';

// DestroyAllDialog is the guarded bulk-destroy flow: an explicit
// acknowledgement step (no bare OK/Cancel), then live progress as machines are
// torn down, then a per-machine success/failure summary. Progress during the
// run is read off the live machines list rather than the mutation itself: each
// destroy publishes an SSE `destroyed` event that drops the machine from the
// cache in real time as the backend works through the batch, ahead of the bulk
// request's own response.
export function DestroyAllDialog({
  machines,
  onClose,
}: {
  machines: MachineSummary[];
  onClose: () => void;
}) {
  const destroyAll = useDestroyAllMachines();
  const [step, setStep] = useState<Step>('confirm');
  const [result, setResult] = useState<DestroyAllResponse | null>(null);
  const totalRef = useRef(machines.length);

  const destroyedSoFar = Math.max(0, totalRef.current - machines.length);
  const inFlight = Math.min(destroyedSoFar + 1, totalRef.current);

  const runDestroy = (force: boolean) => {
    totalRef.current = machines.length;
    setStep('running');
    destroyAll.mutate(force, {
      onSuccess: (res) => {
        setResult(res);
        setStep('done');
      },
      onError: () => setStep('confirm'),
    });
  };
  const onConfirm = () => runDestroy(false);
  // TAV-141: retry the whole batch bypassing a blocked session export. Only
  // the machines still around (the ones that failed) are actually destroyed
  // again — the rest are already gone.
  const onForceRemaining = () => runDestroy(true);

  const exportBlockedRemaining = result?.results.some((r) => !r.ok && r.export_failed) ?? false;

  // Block closing mid-run — losing the dialog would also lose the final summary.
  const handleClose = step === 'running' ? () => {} : onClose;

  return (
    <Modal title="Destroy all machines" onClose={handleClose}>
      {step === 'confirm' && (
        <div className="destroy-all">
          <p className="destroy-all-warning">
            This will permanently destroy all {machines.length} machine
            {machines.length === 1 ? '' : 's'} on your account. Each machine&rsquo;s persistent disk
            is wiped and cannot be recovered.
          </p>
          {destroyAll.isError && (
            <div className="form-error">Could not destroy all machines. Please try again.</div>
          )}
          <div className="modal-actions">
            <button type="button" className="btn-ghost" onClick={onClose}>
              Cancel
            </button>
            <button
              type="button"
              className="btn-danger"
              onClick={onConfirm}
              disabled={machines.length === 0}
            >
              Yes, destroy all
            </button>
          </div>
        </div>
      )}

      {step === 'running' && (
        <div className="destroy-all">
          <p className="destroy-all-progress">
            Destroying machine {inFlight}/{totalRef.current}…
          </p>
          <div
            className="destroy-all-bar"
            role="progressbar"
            aria-valuemin={0}
            aria-valuemax={totalRef.current}
            aria-valuenow={destroyedSoFar}
          >
            <div
              className="destroy-all-bar-fill"
              style={{
                width: `${totalRef.current ? (destroyedSoFar / totalRef.current) * 100 : 100}%`,
              }}
            />
          </div>
        </div>
      )}

      {step === 'done' && result && (
        <div className="destroy-all">
          <p className="destroy-all-summary">
            Destroyed {result.destroyed} of {result.total} machine{result.total === 1 ? '' : 's'}
            {result.failed > 0 ? `, ${result.failed} failed` : ''}.
          </p>
          {result.failed > 0 && (
            <ul className="destroy-all-results">
              {result.results
                .filter((r) => !r.ok)
                .map((r) => (
                  <li key={r.id} className="destroy-all-result-fail">
                    {r.name}: {r.error ?? 'unknown error'}
                  </li>
                ))}
            </ul>
          )}
          {exportBlockedRemaining && (
            <p className="destroy-all-warning">
              Some machines were kept because exporting their Claude sessions failed. Force delete
              to skip the export and destroy them anyway.
            </p>
          )}
          <div className="modal-actions">
            {exportBlockedRemaining && (
              <button type="button" className="btn-danger" onClick={onForceRemaining}>
                Force delete remaining
              </button>
            )}
            <button type="button" className="btn-primary" onClick={onClose}>
              Done
            </button>
          </div>
        </div>
      )}
    </Modal>
  );
}
