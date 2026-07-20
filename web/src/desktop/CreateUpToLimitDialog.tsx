import { useRef, useState } from 'react';
import type { CreateAllResponse, MachineSummary } from '../api/client';
import { useCreateUpToLimit } from '../api/hooks';
import { Modal } from './Modal';

type Step = 'confirm' | 'running' | 'done';

// CreateUpToLimitDialog is the guarded bulk-create flow: an explicit
// confirmation step naming how many machines will be created, then live
// progress as they come up, then a per-machine success/failure summary.
// Progress during the run is read off the live machines list rather than the
// mutation itself: each create publishes an SSE `machine` event that adds the
// machine to the cache in real time as the backend works through the batch,
// mirroring DestroyAllDialog.
export function CreateUpToLimitDialog({
  machines,
  limit,
  onClose,
}: {
  machines: MachineSummary[];
  limit: number;
  onClose: () => void;
}) {
  const createAll = useCreateUpToLimit();
  const [step, setStep] = useState<Step>('confirm');
  const [result, setResult] = useState<CreateAllResponse | null>(null);
  const startCountRef = useRef(machines.length);
  const totalRef = useRef(Math.max(0, limit - machines.length));

  const toCreate = Math.max(0, limit - machines.length);
  // Clamped to totalRef.current: machines.length can grow for reasons unrelated
  // to this batch (another tab creating a machine mid-run), which must not push
  // the progress bar/ARIA value past its own max.
  const createdSoFar = Math.min(
    Math.max(0, machines.length - startCountRef.current),
    totalRef.current,
  );
  const inFlight = Math.min(createdSoFar + 1, totalRef.current);

  const onConfirm = () => {
    startCountRef.current = machines.length;
    totalRef.current = toCreate;
    setStep('running');
    createAll.mutate(undefined, {
      onSuccess: (res) => {
        setResult(res);
        setStep('done');
      },
      onError: () => setStep('confirm'),
    });
  };

  // Block closing mid-run — losing the dialog would also lose the final summary.
  const handleClose = step === 'running' ? () => {} : onClose;

  return (
    <Modal title="Create machines" onClose={handleClose}>
      {step === 'confirm' && (
        <div className="create-all">
          <p className="create-all-info">
            {toCreate > 0
              ? `This will create ${toCreate} machine${toCreate === 1 ? '' : 's'} using the default template, filling your account up to its limit of ${limit}.`
              : `You already have ${machines.length} machine${machines.length === 1 ? '' : 's'}, at your limit of ${limit}.`}
          </p>
          {createAll.isError && (
            <div className="form-error">Could not create machines. Please try again.</div>
          )}
          <div className="modal-actions">
            <button type="button" className="btn-ghost" onClick={onClose}>
              Cancel
            </button>
            <button
              type="button"
              className="btn-primary"
              onClick={onConfirm}
              disabled={toCreate === 0}
            >
              Yes, create {toCreate} machine{toCreate === 1 ? '' : 's'}
            </button>
          </div>
        </div>
      )}

      {step === 'running' && (
        <div className="create-all">
          <p className="create-all-progress">
            Creating machine {inFlight}/{totalRef.current}…
          </p>
          <div
            className="create-all-bar"
            role="progressbar"
            aria-valuemin={0}
            aria-valuemax={totalRef.current}
            aria-valuenow={createdSoFar}
          >
            <div
              className="create-all-bar-fill"
              style={{
                width: `${totalRef.current ? (createdSoFar / totalRef.current) * 100 : 100}%`,
              }}
            />
          </div>
        </div>
      )}

      {step === 'done' && result && (
        <div className="create-all">
          <p className="create-all-summary">
            Created {result.created} of {result.requested} machine
            {result.requested === 1 ? '' : 's'}
            {result.failed > 0 ? `, ${result.failed} failed` : ''}.
          </p>
          {result.failed > 0 && (
            <ul className="create-all-results">
              {result.results
                .filter((r) => !r.ok)
                .map((r, i) => (
                  <li key={r.id ?? i} className="create-all-result-fail">
                    {r.error ?? 'unknown error'}
                  </li>
                ))}
            </ul>
          )}
          <div className="modal-actions">
            <button type="button" className="btn-primary" onClick={onClose}>
              Done
            </button>
          </div>
        </div>
      )}
    </Modal>
  );
}
