import { useEffect, type ReactNode } from 'react';
import { createPortal } from 'react-dom';

// Modal is a centered dialog over a dimmed backdrop. Esc and a backdrop click
// both close it; clicks inside the panel do not propagate. The first focusable
// child receives focus via the browser's default tab order.
//
// It renders through a portal to document.body: the modal is invoked from the
// taskbar, whose `backdrop-filter` makes it a containing block for
// position:fixed — without the portal the backdrop would pin to the 44px
// taskbar instead of the viewport, clipping the dialog off the top.
export function Modal({
  title,
  onClose,
  children,
}: {
  title: string;
  onClose: () => void;
  children: ReactNode;
}) {
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose();
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [onClose]);

  return createPortal(
    <div className="modal-backdrop" onClick={onClose}>
      <div
        className="modal"
        role="dialog"
        aria-modal="true"
        aria-label={title}
        onClick={(e) => e.stopPropagation()}
      >
        <div className="modal-header">
          <h2 className="modal-title">{title}</h2>
          <button className="modal-close" onClick={onClose} aria-label="Close">
            ×
          </button>
        </div>
        <div className="modal-body">{children}</div>
      </div>
    </div>,
    document.body,
  );
}
